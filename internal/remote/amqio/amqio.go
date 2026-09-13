// Package amqio is the AMQ carrier for the remote endpoint: it imports
// commands from the endpoint's own handle mailbox and publishes replies and
// request revisions back as ordinary AMQ messages. It never drains a message
// before the endpoint has handled it, and it never deletes.
package amqio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// DefaultHandle is the endpoint's mailbox handle in the root.
const DefaultHandle = "remote"

// Labels the carrier puts on every message it writes.
const (
	LabelRemote   = "remote"
	labelRefPfx   = "request_ref:"
	labelRevPfx   = "revision:"
	subjectPrefix = "remote request "
)

// Carrier binds one endpoint to one AMQ root and handle.
type Carrier struct {
	root     string
	me       string
	identity fsq.DeliveryRootIdentity
	ep       *core.Endpoint
	now      func() time.Time
	router   ReplyRouter
	// curRecovered is set after the first full cur sweep (the crash-recovery
	// scan). Steady-state reconciliation tracks only IDs THIS process claimed.
	curRecovered bool
	// claimedThisRun tracks cur entries this process claimed but has not yet
	// confirmed a receipt for. If the process dies, the set is lost; the next
	// startup's full sweep catches exactly those.
	claimedThisRun map[string]claimedEntry
	mu             sync.Mutex
	// curSweepCount is a test seam: counts full cur sweeps (recoverCur).
	// Steady-state calls go through recoverClaimed and do NOT increment this.
	curSweepCount int
}

// claimedEntry pairs a cur filename with its message ID for steady-state
// reconciliation: the filename locates the cur entry, the message ID locates
// the receipt (a stat, not a file read).
type claimedEntry struct {
	filename string // cur entry name (same as the new filename)
	msgID    string // message header ID, for the receipt filename
}

// ReplyRouter resolves where a cross-project caller's reply must be written.
// It takes the reply_project and reply_to headers the caller stamped on its
// command and returns the delivery root plus the mailbox handle inside it.
// The carrier deliberately knows nothing about .amqrc, peer maps or session
// layout: cmd/amq-remote injects cli.ResolveReplyRoute, tests inject a fake.
// A nil router means this endpoint serves same-project callers only.
type ReplyRouter func(replyProject, replyTo string) (root, handle string, err error)

// SetReplyRouter installs the cross-project reply resolver. Without it, a
// command carrying reply_project is refused rather than answered into the
// endpoint's own root.
func (c *Carrier) SetReplyRouter(r ReplyRouter) { c.router = r }

// errNoReplyRoute reports that a cross-project reply cannot be routed.
// The error wraps the specific cause. F5 distinguishes POISON (DLQ) from
// TRANSIENT (stays in new): a poison error is about the message itself (no
// router configured, unusable handle), while a transient error is about the
// world right now (peer root or mailbox not available yet).
var errNoReplyRoute = errors.New("cross-project reply cannot be routed")

// TransientRouteError marks a route error as TRANSIENT (the world is not
// ready right now: peer root or mailbox not available). The carrier leaves
// the message in new for the next tick. The router (cli.ResolveReplyRoute)
// wraps its retryable failures in this type; the carrier only reads the
// declaration via errors.As and stops inferring anything from which function
// failed.
type TransientRouteError struct{ err error }

// NewTransientRouteError wraps err as a transient route error. Used by the
// injected ReplyRouter (cli.ResolveReplyRoute) to declare that a routing
// failure is retryable — the peer root may not exist YET, the mailbox may
// not be provisioned YET.
func NewTransientRouteError(err error) error {
	return &TransientRouteError{err: err}
}

func (e *TransientRouteError) Error() string { return e.err.Error() }
func (e *TransientRouteError) Unwrap() error { return e.err }

// isPoisonRoute reports whether a route error is about the MESSAGE (poison,
// DLQ) rather than the WORLD RIGHT NOW (transient, stays in new).
// The router declares transient failures by wrapping them in
// TransientRouteError; everything else is poison. The carrier does NOT infer
// from which function failed (B1: that approach missed the real router's
// errors, which are the only path to peer-root-absent in production).
func isPoisonRoute(err error) bool {
	var tre *TransientRouteError
	return !errors.As(err, &tre)
}

// New prepares the endpoint mailbox under root and returns the carrier.
func New(root, me string, ep *core.Endpoint) (*Carrier, error) {
	if err := fsq.ValidateHandle(me); err != nil {
		return nil, protocol.Refuse(protocol.CodeInvalid, "endpoint handle: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		return nil, fmt.Errorf("prepare mailbox for %s: %w", me, err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return nil, err
	}
	return &Carrier{root: root, me: me, identity: identity, ep: ep, now: time.Now}, nil
}

// Handle is the endpoint's mailbox handle.
func (c *Carrier) Handle() string { return c.me }

// SourceHost derives the authenticated creator host of a command from the
// message header. The handle is attribution inside this root; a cross-project
// sender keeps its project so two same-named handles never share a key.
func SourceHost(h format.Header) string {
	host := "amq:" + h.From
	if h.FromProject != "" {
		host += "@" + h.FromProject
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == ':', r == '-':
			return r
		}
		return '-'
	}, host)
}

// sourceHostFromOrigin rebuilds the authenticated creator host from the
// origin map recoverOne saved — the same derivation SourceHost applies to a
// message header.
func sourceHostFromOrigin(origin map[string]string) string {
	h := format.Header{From: origin["from"], FromProject: origin["from_project"]}
	return SourceHost(h)
}

// ImportOnce reads every message in the endpoint's inbox/new, hands each
// command to the endpoint, and only then claims the message into cur with a
// drained receipt. A message the endpoint refuses is still claimed, with the
// refusal in the receipt detail and a reply to the sender.
//
// D1: a per-message failure is NEVER a loop failure. ImportOnce's job is to
// make progress on every message independently. One caller's bad header must
// not stop another caller's work. Each message is classified as (a) handled,
// (b) transient — leave in new, try next tick (storage refusal), or (c)
// terminal-for-us — cannot ever be answered by this endpoint, DLQ it. Errors
// are accumulated, never abort the scan. Returns the count plus a joined error.
func (c *Carrier) ImportOnce() (int, error) {
	root, err := fsq.OpenDeliveryRoot(c.root, c.identity)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir(filepath.Join("agents", c.me, "inbox", "new"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	n := 0
	var errs []error
	for _, name := range names {
		ok, err := c.importOne(root, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		if ok {
			n++
		}
	}
	// Pro B09 cur recovery: a command claimed into cur that crashed between
	// the claim and its receipt (or whose receipt write failed) is invisible
	// to the new-scan above and would otherwise never be heard from again.
	// Reconcile cur: emit any missing drained receipt, and make sure the
	// caller learned the command's outcome. The command itself is NEVER
	// re-executed — the endpoint owns idempotence through its durable record
	// (an identical resubmit is answered from the record, not re-dispatched),
	// but the bookkeeping must converge.
	//
	// Bounded cost: a FULL sweep runs ONCE on the first ImportOnce (the
	// crash-recovery scan — recovery is a startup condition, not steady
	// state). Thereafter, reconcile only what THIS process claimed into cur
	// during this run. That gives O(cur) once per process and O(claimed-this-
	// tick) thereafter, with no loss of coverage. If the process dies, the
	// in-memory set is lost, and the next startup's full sweep catches
	// exactly those.
	// P2 #5: the first sweep runs regardless of the new-scan result. One
	// persistently failing importOne must NOT defer crash recovery indefinitely.
	// The sweep is decoupled — it runs after the new-scan, not gated on it.
	c.mu.Lock()
	firstSweep := !c.curRecovered
	claimed := make(map[string]claimedEntry, len(c.claimedThisRun))
	for k, v := range c.claimedThisRun {
		claimed[k] = v
	}
	c.mu.Unlock()
	if firstSweep {
		if rerr := c.recoverCur(root); rerr != nil {
			// Accumulate, don't return — the new-scan results are still valid.
			if err == nil {
				err = rerr
			} else {
				err = errors.Join(err, rerr)
			}
		}
		c.mu.Lock()
		c.curRecovered = true
		c.mu.Unlock()
	} else if len(claimed) > 0 {
		if rerr := c.recoverClaimed(root, claimed); rerr != nil {
			if err == nil {
				err = rerr
			} else {
				err = errors.Join(err, rerr)
			}
		}
	}
	// Combine the new-scan errors with the cur-recovery errors.
	scanErr := errors.Join(errs...)
	if scanErr != nil {
		if err == nil {
			err = scanErr
		} else {
			err = errors.Join(err, scanErr)
		}
	}
	return n, err
}

// recoverCur reconciles the endpoint's retained cur bookkeeping: every
// message in inbox/cur must have a drained receipt, and a command message
// that never produced a durable outcome reply must get one now. It reads the
// record the command created (by request_ref from the origin the endpoint
// persisted) rather than re-running the command.
// recoverCur reconciles the endpoint's retained cur bookkeeping: every
// message in inbox/cur must have a drained receipt, and a command message
// whose receipt is missing is recovered (receipt emitted, outcome reply
// reconstructed from the durable record — never re-executed).
//
// P0 #2 (D1 principle): a per-entry failure is NEVER a loop failure. One
// unparseable receipt or read error must not abort the sweep, strand every
// entry sorted after it, and re-run the full O(cur) scan every tick. Skip
// the entry, accumulate the error, finish the sweep.
func (c *Carrier) recoverCur(root *fsq.DeliveryRoot) error {
	c.mu.Lock()
	c.curSweepCount++
	c.mu.Unlock()
	curDir := filepath.Join("agents", c.me, "inbox", "cur")
	entries, err := root.ReadDir(curDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if err := c.recoverOne(root, curDir, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// recoverClaimed reconciles only the cur entries this process claimed during
// this run (steady state). For each, it stats the receipt by message ID — a
// filesystem stat, not a message-file read. If the receipt exists, the claim
// completed and the entry is dropped from the pending set. If not, the entry
// is recovered via recoverOne (the crash gap: we claimed but the receipt
// never landed, likely because of an error between MoveNewToCur and
// EmitDeliveryRoot in THIS process).
func (c *Carrier) recoverClaimed(root *fsq.DeliveryRoot, claimed map[string]claimedEntry) error {
	curDir := filepath.Join("agents", c.me, "inbox", "cur")
	var errs []error
	for id, entry := range claimed {
		receiptPath := filepath.Join("agents", c.me, "receipts", fmt.Sprintf("%s__%s__%s.json", entry.msgID, c.me, receipt.StageDrained))
		if _, err := receipt.ReadDeliveryRoot(root, receiptPath); err == nil {
			// Receipt exists — claim completed. Drop from the pending set.
			c.mu.Lock()
			delete(c.claimedThisRun, id)
			c.mu.Unlock()
			continue
		}
		if err := c.recoverOne(root, curDir, entry.filename); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.filename, err))
			continue
		}
		// recoverOne emitted the receipt (or confirmed it); drop from the set.
		c.mu.Lock()
		delete(c.claimedThisRun, id)
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (c *Carrier) recoverOne(root *fsq.DeliveryRoot, curDir, name string) error {
	path := filepath.Join(c.root, curDir, name)
	msg, err := format.ReadMessageFile(path)
	if err != nil {
		// Unreadable cur entry: not ours to fix (amq tooling owns DLQ).
		return nil
	}
	// Receipt by deterministic filename; WriteFileAtomic is idempotent, so
	// re-emitting an existing receipt is a harmless no-op rewrite.
	hasReceipt := true
	if _, err := receipt.ReadDeliveryRoot(root, filepath.Join("agents", c.me, "receipts", fmt.Sprintf("%s__%s__%s.json", msg.Header.ID, c.me, receipt.StageDrained))); err != nil {
		if !os.IsNotExist(err) {
			// P0 #2: a receipt that EXISTS BUT DOES NOT PARSE must not abort the
			// sweep. The receipt file exists, so the case was closed — skip this
			// entry and continue. amq tooling owns repairing corrupted receipts.
			return nil
		}
		hasReceipt = false
	}
	cmd, derr := protocol.DecodeCommand([]byte(strings.TrimSpace(msg.Body)))
	origin := map[string]string{
		"carrier":       "amq",
		"from":          msg.Header.From,
		"thread":        msg.Header.Thread,
		"msg_id":        msg.Header.ID,
		"reply_to":      msg.Header.ReplyTo,
		"reply_project": msg.Header.ReplyProject,
		"from_project":  msg.Header.FromProject,
	}
	if derr != nil {
		// The claim happened, so the command was handled or refused at the
		// time; without a decodable body we cannot reconstruct a reply, but
		// the missing receipt still must be emitted.
		if !hasReceipt {
			rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, "remote command recovered from cur; body undecodable")
			return receipt.EmitDeliveryRoot(root, rc)
		}
		return nil
	}
	// Reconstruct the outcome from the durable record — never re-execute.
	// The crash gap being closed is between claim and receipt: the record
	// already reflects the command's outcome, so the reply is a read, not a
	// second execution.
	//
	// P0 #1: emit the receipt FIRST, then reply. The receipt is our own
	// bookkeeping and is idempotent (WriteFileAtomic); the reply is someone
	// else's mailbox and is not. A persistent receipt-write failure must not
	// produce an unbounded stream of duplicate replies on every tick.
	//
	// P1 #4: for request ops (submit/cancel), the outcome travels via Publish
	// BEFORE the claim in the real flow. If the record shows PublishedRevision
	// >= Revision, the caller already got the outcome — do NOT send a
	// duplicate reply. For non-request ops (request.get, etc.), the reply is
	// inline and may genuinely be missing, so always reply.
	if !hasReceipt {
		rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, "remote command recovered from cur")
		if err := receipt.EmitDeliveryRoot(root, rc); err != nil {
			return err
		}
		// Receipt emitted — the case is closed. Now reply, unless the outcome
		// was already published (request ops only).
		shouldReply := true
		if cmd.Op == protocol.OpRequestSubmit || cmd.Op == protocol.OpRequestCancel {
			// P1 #4: if the outcome was already published (PublishedRevision >=
			// Revision), the caller got it via Publish before the crash. Do not
			// send a duplicate reply.
			if key, ok := recoveredKey(cmd, origin); ok {
				if rec, found, _ := c.ep.Store().Get(key); found && rec.Revision > 0 && rec.PublishedRevision >= rec.Revision {
					shouldReply = false
				}
			}
		}
		if shouldReply {
			if err := c.reply(root, origin, "remote reply", c.reconstructReply(cmd, origin), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconstructReply rebuilds the caller-facing reply for a claimed command
// from the durable record. It returns the record's snapshot so the caller
// learns the outcome, or a typed refusal when no record exists (the command
// never created one — e.g. it was refused before any persist and the refusal
// reply itself was lost in the crash).
// recoveredKey derives the store key for a recovered command, mirroring
// reconstructReply's key derivation. Returns ok=false when the command has
// neither RequestID nor RequestRef.
func recoveredKey(cmd *protocol.Command, origin map[string]string) (requests.Key, bool) {
	if cmd == nil {
		return requests.Key{}, false
	}
	switch {
	case cmd.RequestID != "":
		return requests.Key{CreatorHost: sourceHostFromOrigin(origin), TargetID: cmd.TargetID, RequestID: cmd.RequestID}, true
	case cmd.RequestRef != "":
		host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
		if err != nil {
			return requests.Key{}, false
		}
		return requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}, true
	default:
		return requests.Key{}, false
	}
}

func (c *Carrier) reconstructReply(cmd *protocol.Command, origin map[string]string) any {
	if cmd == nil {
		return nil
	}
	var key requests.Key
	switch {
	case cmd.RequestID != "":
		// Same host derivation as importOne: the authenticated source from
		// the message header, never a claimed handle.
		key = requests.Key{CreatorHost: sourceHostFromOrigin(origin), TargetID: cmd.TargetID, RequestID: cmd.RequestID}
	case cmd.RequestRef != "":
		host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
		if err != nil {
			return protocol.Refuse(protocol.CodeInvalid, "recovered command has an invalid request_ref")
		}
		key = requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	default:
		return nil
	}
	rec, ok, err := c.ep.Store().Get(key)
	if err != nil {
		return protocol.Refuse(protocol.CodeNativeError, "recovered record read: %v", err)
	}
	if !ok {
		return protocol.Refuse(protocol.CodeNotFound, "no record for recovered request")
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.Op(cmd.Op)}}
}

// importOne handles one message from inbox/new. Returns (handled, err):
// handled=true means the message was claimed into cur (or DLQ'd); handled=false
// means it was left in new for a retry (transient failure, or unparseable).
// err is non-nil for errors that should be accumulated (D1: never aborts the scan).
func (c *Carrier) importOne(root *fsq.DeliveryRoot, name string) (bool, error) {
	path := filepath.Join(c.root, "agents", c.me, "inbox", "new", name)
	msg, err := format.ReadMessageFile(path)
	if err != nil {
		// Unparseable serialization belongs to the DLQ path owned by amq
		// read/drain; the endpoint leaves it in new for that tooling.
		return false, nil
	}
	detail := "remote command handled"
	cmd, derr := protocol.DecodeCommand([]byte(strings.TrimSpace(msg.Body)))
	origin := map[string]string{
		"carrier":       "amq",
		"from":          msg.Header.From,
		"thread":        msg.Header.Thread,
		"msg_id":        msg.Header.ID,
		"reply_to":      msg.Header.ReplyTo,
		"reply_project": msg.Header.ReplyProject,
	}
	// D2: route ONLY when the caller is actually elsewhere. If reply_project
	// is empty, the caller lives in OUR root — take the pre-existing same-root
	// path, with the old tolerant handling of a missing or unusable From. Only
	// a genuinely cross-project caller goes near the ReplyRouter.
	project := strings.TrimSpace(origin["reply_project"])
	if project != "" {
		// F5: POISON vs TRANSIENT. Poison is a statement about the MESSAGE;
		// transient is a statement about the WORLD RIGHT NOW.
		//   - Poison (DLQ): the message names a project we do not know, or
		//     carries a handle that can never be valid. No retry can fix it.
		//   - Transient (stays in new): the peer root does not exist YET, the
		//     mailbox is not provisioned YET, the volume is unavailable. The
		//     next tick may well succeed.
		// D3 originally said "unroutable is poison" — that was too broad. This
		// is the correction, and it is consistent with transientRefusal (below).
		_, _, closeProbe, rerr := c.destination(root, origin)
		if rerr != nil {
			if isPoisonRoute(rerr) {
				// F4: emit a DLQ receipt so a caller using `send --wait-for
				// drained` does not wait forever on a consumed message. Every
				// other DLQ site in this repo pairs the move with a receipt.
				if _, dlqErr := fsq.MoveToDLQ(root, c.me, name, msg.Header.ID, "unroutable", rerr.Error()); dlqErr != nil {
					return false, fmt.Errorf("dlq %s: %w (route error: %v)", name, dlqErr, rerr)
				}
				rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDLQ, "unroutable: "+rerr.Error())
				if rerr2 := receipt.EmitDeliveryRoot(root, rc); rerr2 != nil {
					return false, fmt.Errorf("dlq receipt %s: %w", name, rerr2)
				}
				return true, nil // DLQ'd — the message is no longer in new
			}
			// Transient: the peer root or mailbox is not available YET. Leave
			// the message in new for the next tick.
			return false, nil
		}
		closeProbe()
	}
	var reply any
	var herr error
	if derr != nil {
		herr = derr
	} else {
		reply, herr = c.ep.Handle(cmd, core.Source{Host: SourceHost(msg.Header), Origin: origin})
	}
	var refusal *protocol.Refusal
	typed := herr == nil || errors.As(herr, &refusal)
	if herr != nil {
		detail = "remote command refused: " + herr.Error()
	}
	if !typed {
		// The endpoint could not say whether a record exists. Leave the
		// message in new so the next import retries it; never drain a
		// command whose record may not exist.
		return false, nil
	}
	// A typed Refusal is not automatically a DURABLE one. The store refuses
	// with storage_full (ENOSPC, EPERM, oversize) and store_closed (shutdown),
	// and on every op except submit-create those travel as an ERROR from
	// Handle rather than as an Outcome on a Reply — so checking only
	// Outcome.Code (the previous guard) missed them, and classifying by TYPE
	// answered "refused" and CLAIMED the command for a failure that says
	// nothing about the request. Worst case: a cancel whose tombstone could
	// not be written was answered as refused and claimed, and the later
	// submit then EXECUTED the request the caller had cancelled. Classify by
	// CODE, from either channel (agent-message-queue-611.22.13).
	var outcome protocol.Outcome
	if rep, ok := reply.(protocol.Reply); ok {
		outcome = rep.Outcome
	}
	if transientRefusal(refusal, outcome.Code) {
		return false, nil
	}
	// Reply once here for: every refusal; every non-request op; and a request
	// op that carries an op-specific Outcome which does not travel as a
	// published revision (Pro B09: the caller must still learn the outcome).
	// The signal is a Code OR a Disposition: a no-op terminal cancel carries a
	// disposition with an empty code and never publishes a revision, so keying
	// on Code alone silently dropped its reply.
	isRequestOp := cmd != nil && (cmd.Op == protocol.OpRequestSubmit || cmd.Op == protocol.OpRequestCancel)
	hasOutcomeSignal := outcome.Code != "" || outcome.Disposition != ""
	if herr != nil || !isRequestOp || hasOutcomeSignal {
		if err := c.reply(root, origin, "remote reply", reply, herr); err != nil {
			// D1: a reply-route failure on ONE message must not abort the whole
			// scan. The message is not claimed (stays in new); accumulate the
			// error and continue. If this is a cross-project route failure, the
			// next tick's probe will DLQ it (D3).
			return false, err
		}
	}
	if err := fsq.MoveNewToCur(root, c.me, name); err != nil {
		var committed *fsq.CommittedDurabilityError
		if !errors.As(err, &committed) {
			return false, fmt.Errorf("claim %s: %w", name, err)
		}
	}
	// Track this claim for steady-state cur reconciliation. Dropped after
	// the receipt is confirmed; if we crash between MoveNewToCur and the
	// receipt, the next startup's full sweep catches it.
	c.mu.Lock()
	if c.claimedThisRun == nil {
		c.claimedThisRun = map[string]claimedEntry{}
	}
	c.claimedThisRun[msg.Header.ID] = claimedEntry{filename: name, msgID: msg.Header.ID}
	c.mu.Unlock()
	rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, detail)
	if err := receipt.EmitDeliveryRoot(root, rc); err != nil {
		return false, err
	}
	// Receipt confirmed — drop from the pending set.
	c.mu.Lock()
	delete(c.claimedThisRun, msg.Header.ID)
	c.mu.Unlock()
	return true, nil
}

// Publish implements core.Publisher for records that arrived over AMQ. Local
// CLI records have no AMQ origin and are read back over IPC instead.
func (c *Carrier) Publish(snap protocol.Snapshot, origin map[string]string) error {
	if origin == nil || origin["carrier"] != "amq" {
		return nil
	}
	root, err := fsq.OpenDeliveryRoot(c.root, c.identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return c.reply(root, origin, subjectPrefix+string(snap.State), snap, nil)
}

// destination returns the root the reply must be written to and the handle
// inside it. A same-project caller is answered in this endpoint's own root; a
// cross-project caller is answered in ITS root, resolved through the injected
// router. The returned closer is never nil.
func (c *Carrier) destination(own *fsq.DeliveryRoot, origin map[string]string) (*fsq.DeliveryRoot, string, func(), error) {
	noop := func() {}
	to := origin["from"]
	project := strings.TrimSpace(origin["reply_project"])
	if project == "" {
		// D2: same-project caller. Restore the pre-fix tolerant handling: a
		// missing or unusable From is NOT a route error here — the reply
		// writes to the sender's handle in our own root, and an empty `to`
		// means no reply (the pre-fix code returned nil, not an error).
		if to == "" || fsq.ValidateHandle(to) != nil {
			return own, "", noop, nil
		}
		return own, to, noop, nil
	}
	if c.router == nil {
		return nil, "", noop, fmt.Errorf("%w: no reply router configured for project %q", errNoReplyRoute, project)
	}
	rootPath, handle, err := c.router(project, origin["reply_to"])
	if err != nil {
		// B1: the router declares transient failures by wrapping them in
		// TransientRouteError. We MUST preserve that wrapper — using %v here
		// would flatten it to a string and isPoisonRoute would never see it.
		// If the router already returned a TransientRouteError, pass it through;
		// otherwise wrap in errNoReplyRoute (poison).
		var tre *TransientRouteError
		if errors.As(err, &tre) {
			return nil, "", noop, err
		}
		return nil, "", noop, fmt.Errorf("%w: %v", errNoReplyRoute, err)
	}
	if fsq.ValidateHandle(handle) != nil {
		return nil, "", noop, fmt.Errorf("%w: unusable routed handle %q", errNoReplyRoute, handle)
	}
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return nil, "", noop, &TransientRouteError{fmt.Errorf("%w: %v", errNoReplyRoute, err)}
	}
	peer, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return nil, "", noop, &TransientRouteError{fmt.Errorf("%w: %v", errNoReplyRoute, err)}
	}
	// D4: never create a mailbox in someone else's root. A peer root whose
	// mailbox does not exist is unroutable (D3 applies). Creating it is
	// exactly the black hole this PR set out to close, just moved one dir over.
	// Every other cross-root writer gates on ValidateExistingMailboxLayout first.
	if err := fsq.ValidateExistingMailboxLayout(peer, handle); err != nil {
		_ = peer.Close()
		return nil, "", noop, &TransientRouteError{fmt.Errorf("%w: peer mailbox %q does not exist: %v", errNoReplyRoute, handle, err)}
	}
	return peer, handle, func() { _ = peer.Close() }, nil
}

func (c *Carrier) reply(root *fsq.DeliveryRoot, origin map[string]string, subject string, body any, refusal error) error {
	dest, to, closeDest, rerr := c.destination(root, origin)
	if rerr != nil {
		return rerr
	}
	defer closeDest()
	// F1: an empty handle means NO REPLY IS OWED — destination() returned
	// (own, "", noop, nil) for a same-project caller with an unusable From.
	// This is a SUCCESS, not a delivery to nobody. Without this early return,
	// DeliverToInboxes(dest, []string{""}, ...) fails ValidateHandle(""),
	// the publish error is swallowed, and Reconcile republishes every tick
	// forever.
	if to == "" {
		return nil
	}
	now := c.now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}
	labels := []string{LabelRemote}
	context := map[string]any{}
	var text []byte
	switch {
	case refusal != nil:
		code := protocol.Code("error")
		var r *protocol.Refusal
		if errors.As(refusal, &r) {
			code = r.Code
		}
		context["remote_error"] = map[string]string{"code": string(code), "message": refusal.Error()}
		text, _ = json.MarshalIndent(context["remote_error"], "", "  ")
		subject = "remote reply refused"
	default:
		if snap, ok := body.(protocol.Snapshot); ok {
			labels = append(labels, labelRefPfx+snap.RequestRef, fmt.Sprintf("%s%d", labelRevPfx, snap.Revision))
		}
		text, err = json.MarshalIndent(body, "", "  ")
		if err != nil {
			return err
		}
		context["remote"] = json.RawMessage(text)
	}
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    c.me,
			To:      []string{to},
			Thread:  origin["thread"],
			Subject: subject,
			Created: now.UTC().Format(time.RFC3339Nano),
			Refs:    refsFrom(origin),
			Kind:    "status",
			Labels:  labels,
			Context: context,
		},
		Body: string(text),
	}
	if msg.Header.Thread == "" {
		msg.Header.Thread = "p2p/" + orderedPair(c.me, to)
	}
	data, err := msg.Marshal()
	if err != nil {
		return err
	}
	_, err = fsq.DeliverToInboxes(dest, []string{to}, id+".md", data)
	var committed *fsq.CommittedDurabilityError
	if err != nil && !errors.As(err, &committed) {
		return err
	}
	return nil
}

// transientRefusal reports whether a refusal describes the STORE's inability
// to persist rather than a decision about the request. Such a command must
// stay in inbox/new: it has no durable record, and claiming it would consume
// the caller's request with neither an outcome nor a retry. The code can
// arrive either as a Refusal error (most ops) or as a Reply Outcome
// (submit-create), so both channels are checked.
func transientRefusal(refusal *protocol.Refusal, outcome protocol.Code) bool {
	for _, code := range []protocol.Code{outcome, refusalCode(refusal)} {
		switch code {
		case protocol.CodeStorageFull, protocol.CodeStoreClosed:
			return true
		}
	}
	return false
}

func refusalCode(r *protocol.Refusal) protocol.Code {
	if r == nil {
		return ""
	}
	return r.Code
}

func refsFrom(origin map[string]string) []string {
	if id := origin["msg_id"]; id != "" {
		return []string{id}
	}
	return nil
}

func orderedPair(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "__" + b
}
