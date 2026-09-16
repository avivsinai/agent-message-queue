// Package amqio is the AMQ carrier for the remote endpoint: it imports
// commands from the endpoint's own handle mailbox and publishes replies and
// request revisions back as ordinary AMQ messages. It never drains a message
// before the endpoint has handled it, and it never deletes.
package amqio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// curFailureCap is the consecutive identical-failure count after which a cur
// entry is skipped on subsequent full sweeps (611.22.46). A persistently
// unreadable entry (EACCES/EIO that never clears) is quarantined from the
// sweep so it stops forcing a full O(cur) rescan every tick. A DIFFERENT
// error resets the counter, so a transient EAGAIN that later changes still
// retries. amq tooling owns the actual DLQ; this only stops the rescan churn.
const curFailureCap = 3

// curFailure records the consecutive identical read failures of one cur entry.
type curFailure struct {
	count   int
	lastErr string // errors.err.Error() compared as a string for "identical"
}

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
	// curFailures tracks consecutive identical read failures per cur entry
	// (611.22.46). A persistently unreadable entry (EACCES/EIO that never
	// clears) previously kept curRecovered false forever, forcing a full
	// O(cur) sweep every tick. Once an entry fails identically curFailureCap
	// times, it is skipped on subsequent sweeps (quarantined from the sweep,
	// not from correctness — amq tooling owns DLQ). A DIFFERENT error resets
	// the count so a transient EAGAIN that later changes still retries.
	curFailures map[string]curFailure
	// claimedThisRun tracks cur entries this process claimed but has not yet
	// confirmed a receipt for. If the process dies, the set is lost; the next
	// startup's full sweep catches exactly those.
	claimedThisRun map[string]claimedEntry
	mu             sync.Mutex
	// curSweepCount is a test seam: counts full cur sweeps (recoverCur).
	// Steady-state calls go through recoverClaimed and do NOT increment this.
	curSweepCount int
	// syncDirFaultForTest is a test hook that injects a fault into every
	// DeliveryRoot the carrier opens, so Publish (which opens its own root)
	// can be tested for CommittedDurabilityError propagation (B10).
	syncDirFaultForTest func(dir string) error

	// owedReceipts are DLQ receipts whose message already left inbox/new but
	// whose receipt write failed. Nothing revisits a DLQ'd command, so the
	// obligation is carried here and retried at the top of every ImportOnce;
	// recoverDLQReceipts rebuilds it from the DLQ entries after a restart
	// (agent-message-queue-611.22.36 packet 5b).
	owedReceipts map[string]receipt.Receipt
	// Warn is called for non-fatal durability failures (B12). When a
	// CommittedDurabilityError's SyncDir retry exhausts all attempts, the
	// message IS in the mailbox (rename committed) but its fsync is
	// unconfirmed — the carrier returns nil (PublishedRevision advances),
	// and Warn surfaces the failure so the operator knows. nil = no-op.
	Warn func(error)
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

// SetSyncDirFaultForTest installs a fault into every DeliveryRoot the carrier
// opens. Used by B10 regression tests to force a CommittedDurabilityError
// through the real Publish path (which opens its own root internally).
func (c *Carrier) SetSyncDirFaultForTest(fn func(dir string) error) {
	c.syncDirFaultForTest = fn
}

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
// crossRoot reports whether a reply leaves the endpoint's own root: the
// caller named another project, or another session of this project
// (reply_to carries a session component, handle@session). The CLI sets a
// bare-handle reply_to only together with reply_project, so a bare handle
// with no project is a same-root caller. importOne's route probe and
// destination() must agree on this, so it is one predicate (B7,
// agent-message-queue-611.22.35).
func crossRoot(origin map[string]string) bool {
	return strings.TrimSpace(origin["reply_project"]) != "" || strings.Contains(strings.TrimSpace(origin["reply_to"]), "@")
}

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
	// The creator host is part of the request KEY, so it must be injective:
	// two different senders may never derive the same host. Sanitizing the
	// readable form alone is not — every illegal rune maps to '-', and the
	// "@" joining handle and project is itself illegal, so From="a" with
	// project "b" and a bare From="a-b" both collapsed to "amq:a-b". Two
	// callers then shared one record: the second submit was answered with the
	// first's snapshot, and a cancel from one terminated the other's run.
	// Project names are unvalidated (a config string or a directory basename),
	// so "web app" and "web-app" collapsed the same way.
	//
	// A readable prefix is kept for humans and logs, but identity rests on a
	// digest of the EXACT (from, project) pair with an unambiguous separator,
	// so the mapping is injective by construction.
	readable := sanitizeHostPart(h.From)
	if h.FromProject != "" {
		readable += "." + sanitizeHostPart(h.FromProject)
	}
	return "amq:" + readable + "." + hostDigest(h.From, h.FromProject)
}

// hostDigest is a short, collision-resistant tag over the exact identity
// pair. The length prefix makes the encoding unambiguous: ("ab","c") and
// ("a","bc") hash differently even though a plain concatenation would not.
func hostDigest(from, project string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s|%d:%s", len(from), from, len(project), project)))
	return hex.EncodeToString(sum[:])[:8]
}

func sanitizeHostPart(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '-'
	}, s)
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
	if c.syncDirFaultForTest != nil {
		root.SetSyncDirFaultForTest(c.syncDirFaultForTest)
	}
	defer func() { _ = root.Close() }()
	err = c.flushOwedReceipts(root)
	// The startup sweep runs BEFORE the new scan: otherwise the first tick
	// sweeps the very entries it just claimed and answers them twice
	// (agent-message-queue-611.22.36 packet 6b). Steady state reconciles
	// only the entries this process claimed, after the scan.
	c.mu.Lock()
	firstSweep := !c.curRecovered
	c.mu.Unlock()
	if firstSweep {
		rerr := errors.Join(c.recoverCur(root), c.recoverDLQReceipts(root))
		if rerr != nil {
			err = errors.Join(err, rerr)
		} else {
			c.mu.Lock()
			c.curRecovered = true
			c.mu.Unlock()
		}
	}
	entries, rderr := root.ReadDir(filepath.Join("agents", c.me, "inbox", "new"))
	if rderr != nil {
		if errors.Is(rderr, os.ErrNotExist) {
			return 0, err
		}
		return 0, errors.Join(err, rderr)
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
	claimed := make(map[string]claimedEntry, len(c.claimedThisRun))
	for k, v := range c.claimedThisRun {
		claimed[k] = v
	}
	c.mu.Unlock()
	if !firstSweep && len(claimed) > 0 {
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
		// 611.22.46: skip entries quarantined by identical repeated failures so
		// one permanently unreadable cur entry stops forcing a full O(cur)
		// rescan every tick. The entry stays on disk; amq tooling owns DLQ.
		if c.curFailureCapped(name) {
			continue
		}
		if err := c.recoverOne(root, curDir, name); err != nil {
			c.recordCurFailure(name, err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else {
			c.clearCurFailure(name)
		}
	}
	return errors.Join(errs...)
}

// curFailureCapped reports whether a cur entry has failed identically at least
// curFailureCap times and should be skipped on this sweep (611.22.46). Caller
// holds c.mu is NOT required — this method locks.
func (c *Carrier) curFailureCapped(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.curFailures[name]
	return ok && f.count >= curFailureCap
}

// recordCurFailure increments the consecutive identical-failure count for a
// cur entry. A DIFFERENT error resets the count to 1 and records the new error
// string, so a transient EAGAIN that later changes still retries (611.22.46).
func (c *Carrier) recordCurFailure(name string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.curFailures == nil {
		c.curFailures = make(map[string]curFailure)
	}
	msg := err.Error()
	f := c.curFailures[name]
	if f.lastErr == msg {
		f.count++
	} else {
		f = curFailure{count: 1, lastErr: msg}
	}
	c.curFailures[name] = f
}

// clearCurFailure resets the failure count for a cur entry that succeeded on
// this sweep (611.22.46). A transient fault that clears resumes normal
// scanning immediately.
func (c *Carrier) clearCurFailure(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.curFailures, name)
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
			// B8: receipt exists, but the reply may not have been delivered.
			// recoverOne writes the receipt BEFORE sending the reply, so a
			// receipt-only state means the reply was lost. Re-run recoverOne
			// to re-send the idempotent recovery reply. Only delete from the
			// pending set when recoverOne succeeds (receipt + reply both done).
			if err := c.recoverOne(root, curDir, entry.filename); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", entry.filename, err))
				continue
			}
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
	msg, err := format.ReadMessageFileRoot(root, filepath.Join(curDir, name))
	if err != nil {
		// A malformed entry is not ours to fix (amq tooling owns DLQ) and an
		// entry that vanished owes nothing. A FAILED read is the world being
		// unavailable right now: report it so the sweep is retried
		// (agent-message-queue-611.22.36 packet 6a).
		if !readFailed(err) {
			return nil
		}
		return err
	}
	// A receipt we can READ proves the case was closed. A receipt that is
	// MISSING, or that EXISTS BUT DOES NOT PARSE, proves nothing: an
	// unparseable receipt cannot tell us the case was closed, and presence is
	// not proof (same error class as "an existing directory is a durable
	// directory" from the fsq work). Treat it exactly like a missing one.
	// recoverOne re-emits the receipt unconditionally (WriteFileAtomic
	// overwrites, repairing the corruption as a side effect) and sends an
	// idempotent recovery reply, so recovery is always safe.
	hasReceipt := true
	if _, err := receipt.ReadDeliveryRoot(root, filepath.Join("agents", c.me, "receipts", fmt.Sprintf("%s__%s__%s.json", msg.Header.ID, c.me, receipt.StageDrained))); err != nil {
		// Missing OR unparseable: recover. An unparseable receipt cannot prove
		// the case was closed, and presence is not proof. recoverOne re-emits
		// the receipt (overwriting any corruption) and sends an idempotent
		// recovery reply (deterministic message id), so recovery is always safe.
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
	// Receipt first, before the ledger. The claim happened, so the command was
	// handled or refused at the time and the drained receipt is owed whatever
	// the ledger says. The ledger arms below return without reaching the
	// later emit, and every claimed command has a ledger entry, so emitting
	// here is the only place that closes the claim-to-receipt crash gap
	// (gate finding on agent-message-queue-611.22.36, union 42d923a4). The
	// receipt is idempotent: WriteFileAtomic overwrites.
	if !hasReceipt {
		detail := "remote command recovered from cur"
		if derr != nil {
			detail += "; body undecodable"
		}
		rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, detail)
		if err := receipt.EmitDeliveryRoot(root, rc); err != nil {
			return err
		}
	}
	// The per-command reply ledger is the authority on what this command was
	// answered with (a refusal for an undecodable body included) and whether
	// that answer became visible. It is written
	// before the claim, so a crash anywhere after it re-sends identical bytes
	// under the same id (agent-message-queue-611.22.36 packets 6b and .37).
	if led, lerr := c.readLedger(msg.Header.ID); lerr != nil {
		return lerr
	} else if led != nil {
		if led.Sent {
			return nil
		}
		return c.deliverLedgered(root, origin, msg.Header.ID, led)
	}
	if derr != nil {
		// Without a decodable body there is no reply to reconstruct; the
		// receipt above is all that was owed.
		return nil
	}
	// Reconstruct the outcome from the durable record — never re-execute.
	// The crash gap being closed is between claim and receipt: the record
	// already reflects the command's outcome, so the reply is a read, not a
	// second execution.
	//
	// The receipt was emitted above, before the ledger and the reply.
	//
	// The reply is NOT gated on !hasReceipt. The receipt-first ordering used
	// to gate the reply inside !hasReceipt, which created a lost-reply window:
	// if the receipt landed but the reply did not (crash between the two),
	// the next sweep saw hasReceipt=true and skipped the entry forever — the
	// caller never got the outcome. The recovery reply is idempotent by
	// construction (deterministic message id + resolvePublishCollision), so a
	// duplicate reply is free. ALWAYS attempt the reply, subject only to the
	// PublishedRevision check (a separate and correct concern: if the outcome
	// already went out via Publish, there is nothing to re-send).
	//
	// The treat-as-missing behaviour for unparseable receipts is retained not
	// because our fsync can corrupt it (it cannot — writeAndSync writes and
	// fsyncs the full content before rename), but because presence is not
	// proof and recovery is free.
	// Always attempt the recovery reply (idempotent), unless the outcome was
	// already published via Publish before the crash (request ops only).
	if cmd.Op != protocol.OpRequestSubmit && cmd.Op != protocol.OpRequestCancel {
		// No ledger entry: the command was claimed but never answered. Answer
		// it now through the same path importOne uses; these ops are reads or
		// idempotent by their own Answered bookkeeping.
		reply, herr := c.ep.Handle(cmd, core.Source{Host: SourceHost(msg.Header), Origin: origin})
		var refusal *protocol.Refusal
		if herr != nil && !errors.As(herr, &refusal) {
			return herr
		}
		return c.answer(root, origin, msg.Header.ID, msg.Header.Created, reply, herr)
	}
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
		body, berr := c.reconstructReply(cmd, origin)
		if berr != nil {
			return berr
		}
		if body == nil {
			// Nothing to say is not an answer; never send a null body.
			return nil
		}
		if err := c.replyWithRecovery(root, origin, "remote reply", body, nil, msg.Header.ID, msg.Header.Created); err != nil {
			return err
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

func (c *Carrier) reconstructReply(cmd *protocol.Command, origin map[string]string) (any, error) {
	if cmd == nil {
		return nil, nil
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
			return protocol.Refuse(protocol.CodeInvalid, "recovered command has an invalid request_ref"), nil
		}
		key = requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	default:
		return nil, nil
	}
	rec, ok, err := c.ep.Store().Get(key)
	if err != nil {
		// A failed read of our own record is not an outcome the caller can be
		// told; leave the entry unrecovered and retry (packet 6a).
		return nil, fmt.Errorf("recovered record read: %w", err)
	}
	if !ok {
		return protocol.Refuse(protocol.CodeNotFound, "no record for recovered request"), nil
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.Op(cmd.Op)}}, nil
}

// importOne handles one message from inbox/new. Returns (handled, err):
// handled=true means the message was claimed into cur (or DLQ'd); handled=false
// means it was left in new for a retry (transient failure, or unparseable).
// err is non-nil for errors that should be accumulated (D1: never aborts the scan).
func (c *Carrier) importOne(root *fsq.DeliveryRoot, name string) (bool, error) {
	msg, err := format.ReadMessageFileRoot(root, filepath.Join("agents", c.me, "inbox", "new", name))
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
	// D2: probe the route ONLY when the caller is actually elsewhere (another
	// project, or another session of this project — B7). A same-root caller
	// takes the pre-existing path with the old tolerant handling of a missing
	// or unusable From and never goes near the ReplyRouter. Probing BEFORE
	// Handle means a prompt whose reply can never be delivered is not run.
	if crossRoot(origin) {
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
					// The move succeeded: the command is gone from new and nothing
					// revisits DLQ. The receipt is now an owed obligation (packet 5b).
					c.mu.Lock()
					if c.owedReceipts == nil {
						c.owedReceipts = map[string]receipt.Receipt{}
					}
					c.owedReceipts[msg.Header.ID] = rc
					c.mu.Unlock()
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
	owesReply := herr != nil || !isRequestOp || hasOutcomeSignal
	// Order: ledger, claim, receipt, reply. A reply sent before the claim is
	// re-sent under a fresh id when the claim fails (.37); a claim before the
	// ledger leaves a crash with nothing to re-send from. The ledger is the
	// exact bytes, so every later attempt is byte-identical and a collision
	// with an earlier attempt resolves as a no-op. A route failure here leaves
	// the message in new for the next tick (D1).
	if owesReply {
		if err := c.recordAnswer(root, origin, msg.Header.ID, msg.Header.Created, reply, herr); err != nil {
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
	if owesReply {
		led, lerr := c.readLedger(msg.Header.ID)
		if lerr != nil {
			return false, lerr
		}
		if led != nil && !led.Sent {
			if err := c.deliverLedgered(root, origin, msg.Header.ID, led); err != nil {
				// Claimed and receipted; the pending set carries the reply
				// obligation to recoverClaimed on the next tick.
				return false, err
			}
		}
	}
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
	if c.syncDirFaultForTest != nil {
		root.SetSyncDirFaultForTest(c.syncDirFaultForTest)
	}
	defer func() { _ = root.Close() }()
	return c.replyWith(root, origin, subjectPrefix+string(snap.State), snap, nil)
}

// destination returns the root the reply must be written to and the handle
// inside it. A same-project caller is answered in this endpoint's own root; a
// cross-project caller is answered in ITS root, resolved through the injected
// router. The returned closer is never nil.
func (c *Carrier) destination(own *fsq.DeliveryRoot, origin map[string]string) (*fsq.DeliveryRoot, string, func(), error) {
	noop := func() {}
	to := origin["from"]
	project := strings.TrimSpace(origin["reply_project"])
	replyTo := strings.TrimSpace(origin["reply_to"])
	if project == "" {
		if !crossRoot(origin) {
			// Same-root caller: a missing or unusable From is not a route
			// error; an empty handle means no reply is owed.
			if to == "" || fsq.ValidateHandle(to) != nil {
				return own, "", noop, nil
			}
			return own, to, noop, nil
		}
		// B7: another session of this project. Route through the router; a
		// failure propagates (transient stays transient), never a silent
		// fallback to our own root, which would deliver the reply to the
		// wrong session.
		if c.router == nil {
			return nil, "", noop, fmt.Errorf("%w: no reply router configured for cross-session reply to %q", errNoReplyRoute, replyTo)
		}
		rootPath, handle, err := c.router("", replyTo)
		if err != nil {
			// The router already classified the failure: an absent session root
			// is transient (it can be created later); anything else is poison.
			var tre *TransientRouteError
			if errors.As(err, &tre) {
				return nil, "", noop, err
			}
			return nil, "", noop, fmt.Errorf("%w: cross-session reply to %q: %v", errNoReplyRoute, replyTo, err)
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
		// D4 (agent-message-queue-611.22.43): the cross-project arm below gates
		// on ValidateExistingMailboxLayout so a peer root whose mailbox does
		// not exist is never written into (no black-hole creation in someone
		// else's root). The cross-session arm must do the same — a routed
		// session root with a missing mailbox is unroutable, not a write
		// target. Without this gate, a cross-session reply silently created
		// the mailbox in the peer root, the very hole D4 closed one dir over.
		if err := fsq.ValidateExistingMailboxLayout(peer, handle); err != nil {
			_ = peer.Close()
			return nil, "", noop, &TransientRouteError{fmt.Errorf("%w: peer mailbox %q does not exist: %v", errNoReplyRoute, handle, err)}
		}
		return peer, handle, func() { _ = peer.Close() }, nil
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

// replyWith delivers a reply message. On CommittedDurabilityError (visible
// rename, unknown fsync), the fsync is retried in place (B12). The message is
// already in the mailbox; re-delivery after consumption is never idempotent.
func (c *Carrier) replyWith(root *fsq.DeliveryRoot, origin map[string]string, subject string, body any, refusal error) error {
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
	var id string
	var created string
	var err error
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
	// B1: for Publish (revision publication), use a DETERMINISTIC message id
	// derived from (request_ref, revision) so a republish of the same
	// immutable revision resolves to the SAME filename and identical bytes.
	// resolvePublishCollision returns nil on a byte-identical collision, so
	// every retry after the first is a genuine no-op — at-least-once delivery
	// with an idempotent write is exactly-once in effect, and it needs no
	// cooperation from the receiver. The Created timestamp is also stable
	// (derived from the revision, not wall-clock) so the bytes never drift.
	// The inline reply keeps the fresh id — it has no retry, so amplification
	// is impossible.
	isPublish := body != nil && refusal == nil
	if isPublish {
		if snap, ok := body.(protocol.Snapshot); ok {
			// B1 + ordering: the id must be deterministic per (request_ref,
			// revision) AND sort chronologically against ordinary AMQ message
			// ids (<RFC3339>_pid<N>_<rand>). A bare "publish__" prefix sorts
			// AFTER every 2026-* message, starving drain --limit 20. Fix:
			// stable timestamp first (from snap.ObservedAt, not time.Now —
			// ObservedAt is identical on every retry of that revision, keeping
			// the write idempotent), then the deterministic part, then a short
			// digest of the request_ref so the filename stays short.
			observed, perr := protocol.ParseTime(snap.ObservedAt)
			if perr != nil {
				observed = now
			}
			stamp := observed.UTC().Format("2006-01-02T15:04:05.000Z")
			refDigest := requests.Digest([]byte(snap.RequestRef))
			refDigest = strings.TrimPrefix(refDigest, "sha256:")[:8]
			id = fmt.Sprintf("%s_publish_rev%d_%s", stamp, snap.Revision, refDigest)
			created = protocol.FormatTime(observed)
		} else {
			var err error
			id, err = format.NewMessageID(now)
			if err != nil {
				return err
			}
			created = now.UTC().Format(time.RFC3339Nano)
		}
	} else {
		var err error
		id, err = format.NewMessageID(now)
		if err != nil {
			return err
		}
		created = now.UTC().Format(time.RFC3339Nano)
	}
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    c.me,
			To:      []string{to},
			Thread:  origin["thread"],
			Subject: subject,
			Created: created,
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
	return c.finalizeDelivery(dest, to, id, data)
}

// replyWithRecovery delivers a recovery reply with a DETERMINISTIC message id
// derived from the command's message id, so recovering the same command twice
// produces the IDENTICAL filename and bytes and resolvePublishCollision
// returns nil on the second pass. The id shape mirrors B10's Publish id
// (timestamp prefix for drain ordering, then a deterministic body) but uses
// the command's message id as the digest source. Uses the same reply path
// (same as the inline reply) — the recovery path has no retry amplification
// because the id is already idempotent.
func (c *Carrier) replyWithRecovery(root *fsq.DeliveryRoot, origin map[string]string, subject string, body any, refusal error, cmdMsgID, msgCreated string) error {
	dest, to, closeDest, rerr := c.destination(root, origin)
	if rerr != nil {
		return rerr
	}
	defer closeDest()
	if to == "" {
		return nil
	}
	now := c.now()
	labels := []string{LabelRemote}
	context := map[string]any{}
	var text []byte
	var err error
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
	// Deterministic recovery id: timestamp prefix (for drain --limit ordering)
	// + "recover" + short digest of the command message id. The timestamp is
	// derived from the ORIGINAL message's Created, not time.Now, so a second
	// recovery of the same command mints the IDENTICAL id —
	// resolvePublishCollision returns nil on byte-identical data. A second
	// recovery is a genuine no-op.
	msgDigest := requests.Digest([]byte(cmdMsgID))
	msgDigest = strings.TrimPrefix(msgDigest, "sha256:")[:8]
	recovered, perr := protocol.ParseTime(msgCreated)
	if perr != nil {
		recovered = now
	}
	stamp := recovered.UTC().Format("2006-01-02T15:04:05.000Z")
	// B11: include the revision in the recovery reply ID so different
	// revisions of the same command produce different filenames. Without
	// this, a recovery that publishes the running snapshot and a later
	// recovery that publishes the completed snapshot would collide on the
	// same filename with different bytes — resolvePublishCollision rejects
	// non-byte-identical collisions and retains a conflict temp file.
	// B11: derive the suffix from the content hash (first 12 hex of
	// sha256(encoded body)), not a type switch. Identical content -> same
	// id -> no-op; different content -> both delivered. Handles refusal
	// errors (protocol.Refuse) whose text varies.
	bodyBytes, _ := json.Marshal(body)
	if refusal != nil {
		bodyBytes = []byte(refusal.Error())
	}
	contentHash := sha256.Sum256(bodyBytes)
	revSuffix := hex.EncodeToString(contentHash[:])[:12]
	id := fmt.Sprintf("%s_reply_%s_%s", stamp, msgDigest, revSuffix)
	created := recovered.UTC().Format(time.RFC3339Nano)
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    c.me,
			To:      []string{to},
			Thread:  origin["thread"],
			Subject: subject,
			Created: created,
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
	return c.finalizeDelivery(dest, to, id, data)
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

// finalizeDelivery is the one committed-delivery tail for every reply the
// carrier sends: publish, inline, recovery. "Visible == published" (B12,
// agent-message-queue-611.22.35): a rename that succeeded but whose directory
// fsync is unconfirmed is repaired in place, never re-delivered, because the
// path is the message identity and re-delivery after consumption can never be
// idempotent. Persistent failure is reported through Warn and still counts as
// delivered.
func (c *Carrier) finalizeDelivery(dest *fsq.DeliveryRoot, to, id string, data []byte) error {
	_, err := fsq.DeliverToInboxes(dest, []string{to}, id+".md", data)
	var committed *fsq.CommittedDurabilityError
	if err == nil || !errors.As(err, &committed) {
		return err
	}
	relPath := filepath.Join("agents", to, "inbox", "new", id+".md")
	newDir := filepath.Dir(relPath)
	for attempt := 0; attempt < 3; attempt++ {
		if syncErr := dest.SyncDir(newDir); syncErr == nil {
			return nil
		}
	}
	if c.Warn != nil {
		c.Warn(fmt.Errorf("%s: unconfirmed fsync for %s after 3 attempts: %w", to, relPath, committed))
	}
	return nil
}

// readFailed distinguishes a read that FAILED (the world is unavailable:
// retry) from an entry that is absent or malformed (nothing to retry).
func readFailed(err error) bool {
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, format.ErrMessageTooLarge) {
		return false
	}
	var pe *fs.PathError
	return errors.As(err, &pe)
}

// replyRecord is one entry of the per-command reply ledger: the exact bytes a
// command was answered with, under which id, and whether that answer became
// visible in the caller's inbox. Written before the claim, marked after the
// delivery. It lives under the endpoint's own extension directory
// (agents/<me>/extensions/remote/replies) and is never deleted automatically;
// amq cleanup owns retention.
type replyRecord struct {
	ID   string `json:"id"`
	To   string `json:"to"`
	Data []byte `json:"data"`
	Sent bool   `json:"sent"`
}

func (c *Carrier) ledgerDir() string {
	return filepath.Join(c.root, "agents", c.me, "extensions", "remote", "replies")
}

// validateLedgerID guards the reply ledger against a malformed message id:
// the ledger file is built as <msgID>.json under ledgerDir, so an id carrying
// a path separator, a dotfile prefix, "..", a NUL byte, or an absolute path
// would escape the ledger directory (path traversal) or collide with the
// wrong file. readLedger and writeLedger MUST both call this — writeLedger
// had no validation at all, and readLedger returned (nil, nil) on an invalid
// id, silently dropping a ledger that should have been re-delivered
// (agent-message-queue-611.22.44). The id is the base name only; the .json
// suffix is constructed internally and never taken from the caller.
func validateLedgerID(msgID string) error {
	if msgID == "" {
		return fmt.Errorf("empty ledger id")
	}
	if strings.HasPrefix(msgID, ".") {
		return fmt.Errorf("dotfile ledger id is not allowed")
	}
	if filepath.IsAbs(msgID) {
		return fmt.Errorf("absolute ledger id is not allowed")
	}
	if strings.Contains(msgID, "/") || strings.Contains(msgID, "\\") {
		return fmt.Errorf("path separators are not allowed in ledger id")
	}
	if strings.Contains(msgID, "\x00") {
		return fmt.Errorf("NUL byte is not allowed in ledger id")
	}
	if msgID == "." || msgID == ".." {
		return fmt.Errorf("path traversal is not allowed in ledger id")
	}
	return nil
}

func (c *Carrier) readLedger(msgID string) (*replyRecord, error) {
	if err := validateLedgerID(msgID); err != nil {
		// 611.22.44: do NOT return (nil, nil) — that silently dropped a
		// ledger lookup for a malformed id, so a reply that should have
		// been re-delivered was treated as "no ledger" and a fresh one
		// written over it. Surface the error so the caller can decide.
		return nil, fmt.Errorf("reply ledger id: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(c.ledgerDir(), msgID+".json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reply ledger %s: %w", msgID, err)
	}
	var rec replyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("reply ledger %s: %w", msgID, err)
	}
	return &rec, nil
}

func (c *Carrier) writeLedger(msgID string, rec *replyRecord) error {
	if err := validateLedgerID(msgID); err != nil {
		// 611.22.44: writeLedger had NO filename validation — an id carrying
		// a path separator or ".." would write outside ledgerDir (path
		// traversal). Reject before touching the filesystem.
		return fmt.Errorf("reply ledger id: %w", err)
	}
	if err := os.MkdirAll(c.ledgerDir(), 0o700); err != nil {
		return fmt.Errorf("reply ledger: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := fsq.WriteFileAtomic(c.ledgerDir(), msgID+".json", data, 0o600); err != nil {
		return fmt.Errorf("reply ledger %s: %w", msgID, err)
	}
	return nil
}

// composeReply builds the caller-facing reply message for a command with a
// DETERMINISTIC id: <command created>_reply_<command msg digest>_<body digest>.
// Identical content re-sent for the same command lands on the same filename
// and resolves as a no-op collision; different content (a later revision, a
// different refusal) lands beside it (B11).
func (c *Carrier) composeReply(origin map[string]string, to, subject string, body any, refusal error, cmdMsgID, msgCreated string) (string, []byte, error) {
	now := c.now()
	labels := []string{LabelRemote}
	context := map[string]any{}
	var text []byte
	var err error
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
			return "", nil, err
		}
		context["remote"] = json.RawMessage(text)
	}
	msgDigest := requests.Digest([]byte(cmdMsgID))
	msgDigest = strings.TrimPrefix(msgDigest, "sha256:")[:8]
	created, perr := protocol.ParseTime(msgCreated)
	if perr != nil {
		created = now
	}
	bodyBytes, _ := json.Marshal(body)
	if refusal != nil {
		bodyBytes = []byte(refusal.Error())
	}
	contentHash := sha256.Sum256(bodyBytes)
	id := fmt.Sprintf("%s_reply_%s_%s", created.UTC().Format("2006-01-02T15:04:05.000Z"), msgDigest, hex.EncodeToString(contentHash[:])[:12])
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    c.me,
			To:      []string{to},
			Thread:  origin["thread"],
			Subject: subject,
			Created: created.UTC().Format(time.RFC3339Nano),
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
		return "", nil, err
	}
	return id, data, nil
}

// recordAnswer writes the ledger entry for a command's answer without sending
// it. importOne calls it before the claim; deliverLedgered sends it after the
// receipt, and recoverOne re-sends it after a crash.
func (c *Carrier) recordAnswer(root *fsq.DeliveryRoot, origin map[string]string, cmdMsgID, msgCreated string, body any, refusal error) error {
	_, to, closeDest, rerr := c.destination(root, origin)
	if rerr != nil {
		return rerr
	}
	closeDest()
	if to == "" {
		return nil // no reply is owed (F1); nothing to record
	}
	id, data, err := c.composeReply(origin, to, "remote reply", body, refusal, cmdMsgID, msgCreated)
	if err != nil {
		return err
	}
	return c.writeLedger(cmdMsgID, &replyRecord{ID: id, To: to, Data: data})
}

// answer records and delivers a command's reply in one step (recovery of a
// command that was claimed but never answered).
func (c *Carrier) answer(root *fsq.DeliveryRoot, origin map[string]string, cmdMsgID, msgCreated string, body any, refusal error) error {
	if err := c.recordAnswer(root, origin, cmdMsgID, msgCreated, body, refusal); err != nil {
		return err
	}
	led, err := c.readLedger(cmdMsgID)
	if err != nil || led == nil {
		return err
	}
	return c.deliverLedgered(root, origin, cmdMsgID, led)
}

// deliverLedgered sends a ledgered reply and marks it sent. The destination is
// re-resolved so a session created after the ledger was written is found.
func (c *Carrier) deliverLedgered(root *fsq.DeliveryRoot, origin map[string]string, cmdMsgID string, led *replyRecord) error {
	dest, to, closeDest, rerr := c.destination(root, origin)
	if rerr != nil {
		return rerr
	}
	defer closeDest()
	if to == "" {
		return nil
	}
	if err := c.finalizeDelivery(dest, to, led.ID, led.Data); err != nil {
		return err
	}
	led.Sent = true
	return c.writeLedger(cmdMsgID, led)
}

// flushOwedReceipts retries DLQ receipts whose write failed after the move.
func (c *Carrier) flushOwedReceipts(root *fsq.DeliveryRoot) error {
	c.mu.Lock()
	owed := make(map[string]receipt.Receipt, len(c.owedReceipts))
	for k, v := range c.owedReceipts {
		owed[k] = v
	}
	c.mu.Unlock()
	var errs []error
	for id, rc := range owed {
		if err := receipt.EmitDeliveryRoot(root, rc); err != nil {
			errs = append(errs, fmt.Errorf("owed dlq receipt %s: %w", id, err))
			continue
		}
		c.mu.Lock()
		delete(c.owedReceipts, id)
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

// recoverDLQReceipts rebuilds missing DLQ receipts from the DLQ entries
// themselves after a restart: the entry is durable and carries the original
// message, so a DLQ entry with no receipt is a receipt we still owe.
func (c *Carrier) recoverDLQReceipts(root *fsq.DeliveryRoot) error {
	var errs []error
	for _, box := range []string{fsq.BoxNew, fsq.BoxCur} {
		dir := filepath.Join("agents", c.me, "dlq", box)
		entries, err := root.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			env, original, rerr := fsq.ReadDLQEnvelope(root, filepath.Join(dir, e.Name()))
			if rerr != nil || env == nil || env.OriginalID == "" {
				continue // malformed DLQ entries belong to the DLQ tooling
			}
			receiptPath := filepath.Join("agents", c.me, "receipts", fmt.Sprintf("%s__%s__%s.json", env.OriginalID, c.me, receipt.StageDLQ))
			if _, rerr := receipt.ReadDeliveryRoot(root, receiptPath); rerr == nil {
				continue
			}
			thread, from := "", ""
			if om, perr := format.ParseMessage(original); perr == nil {
				thread, from = om.Header.Thread, om.Header.From
			}
			rc := receipt.New(env.OriginalID, thread, from, c.me, receipt.StageDLQ, env.FailureReason+": "+env.FailureDetail)
			if err := receipt.EmitDeliveryRoot(root, rc); err != nil {
				errs = append(errs, fmt.Errorf("dlq receipt %s: %w", env.OriginalID, err))
			}
		}
	}
	return errors.Join(errs...)
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
