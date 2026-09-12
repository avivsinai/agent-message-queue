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
	for _, name := range names {
		if err := c.importOne(root, name); err != nil {
			return n, err
		}
		n++
	}
	// Pro B09 cur recovery: a command claimed into cur that crashed between
	// the claim and its receipt (or whose receipt write failed) is invisible
	// to the new-scan above and would otherwise never be heard from again.
	// Reconcile cur: emit any missing drained receipt, and make sure the
	// caller learned the command's outcome. The command itself is NEVER
	// re-executed — the endpoint owns idempotence through its durable record
	// (an identical resubmit is answered from the record, not re-dispatched),
	// but the bookkeeping must converge.
	if err := c.recoverCur(root); err != nil {
		return n, err
	}
	return n, nil
}

// recoverCur reconciles the endpoint's retained cur bookkeeping: every
// message in inbox/cur must have a drained receipt, and a command message
// that never produced a durable outcome reply must get one now. It reads the
// record the command created (by request_ref from the origin the endpoint
// persisted) rather than re-running the command.
func (c *Carrier) recoverCur(root *fsq.DeliveryRoot) error {
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
	for _, name := range names {
		if err := c.recoverOne(root, curDir, name); err != nil {
			return err
		}
	}
	return nil
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
			return fmt.Errorf("read receipt for %s: %w", name, err)
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
	isRequestOp := cmd != nil && (cmd.Op == protocol.OpRequestSubmit || cmd.Op == protocol.OpRequestCancel)
	// Emit the outcome reply only when no receipt exists: a missing receipt
	// is the crash signature (everything after the endpoint handled the
	// command may be missing — receipt AND reply), while an existing receipt
	// means the original run completed its replies before crashing.
	if !hasReceipt {
		if isRequestOp {
			if err := c.reply(root, origin, "remote reply", c.reconstructReply(cmd, origin), nil); err != nil {
				return err
			}
		}
		rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, "remote command recovered from cur")
		return receipt.EmitDeliveryRoot(root, rc)
	}
	return nil
}

// reconstructReply rebuilds the caller-facing reply for a claimed command
// from the durable record. It returns the record's snapshot so the caller
// learns the outcome, or a typed refusal when no record exists (the command
// never created one — e.g. it was refused before any persist and the refusal
// reply itself was lost in the crash).
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

func (c *Carrier) importOne(root *fsq.DeliveryRoot, name string) error {
	path := filepath.Join(c.root, "agents", c.me, "inbox", "new", name)
	msg, err := format.ReadMessageFile(path)
	if err != nil {
		// Unparseable serialization belongs to the DLQ path owned by amq
		// read/drain; the endpoint leaves it in new for that tooling.
		return nil
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
		return nil
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
		return nil
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
			return err
		}
	}
	if err := fsq.MoveNewToCur(root, c.me, name); err != nil {
		var committed *fsq.CommittedDurabilityError
		if !errors.As(err, &committed) {
			return fmt.Errorf("claim %s: %w", name, err)
		}
	}
	rc := receipt.New(msg.Header.ID, msg.Header.Thread, msg.Header.From, c.me, receipt.StageDrained, detail)
	return receipt.EmitDeliveryRoot(root, rc)
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

func (c *Carrier) reply(root *fsq.DeliveryRoot, origin map[string]string, subject string, body any, refusal error) error {
	to := origin["from"]
	if to == "" || fsq.ValidateHandle(to) != nil {
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
	_, err = fsq.DeliverToInboxes(root, []string{to}, id+".md", data)
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
