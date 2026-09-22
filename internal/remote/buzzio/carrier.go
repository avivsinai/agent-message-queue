package buzzio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Kinds the edge publishes (relay design §4; enrolled via share --enable
// buzz-dm).
const (
	KindDM   = 9
	KindEdit = 40003
)

// maxRowText bounds one row's text; longer results show an explicit
// shortened-view marker and the ref, never a silent truncation.
const maxRowText = 4000

// ErrClockBehind means the next edit of a row cannot yet be dated strictly
// after the previous one without dating it in the future. The publication
// stays owed and is retried.
var ErrClockBehind = errors.New("edit waits for the clock to pass the previous edit's second")

// Handler is the endpoint's command entry point (core.Endpoint.Handle).
type Handler func(cmd *protocol.Command, src core.Source) (any, error)

// Publisher sends one signed event and returns nil only on the relay's
// matching positive OK (internal/relay Conn.Publish).
type Publisher func(ctx context.Context, evt nostr.Event) error

// Carrier is one shared session's owner-DM edge: it turns verified owner
// events into endpoint commands and endpoint snapshots into one editable
// result row per request.
type Carrier struct {
	ledger  *Ledger
	binding Binding
	secret  [32]byte
	handle  Handler
	now     func() time.Time

	mu sync.Mutex // serializes row preparation (edit dating) and flush
}

// NewCarrier builds the edge for one binding. secret is the body key.
func NewCarrier(ledger *Ledger, b Binding, secret [32]byte, handle Handler) *Carrier {
	return &Carrier{ledger: ledger, binding: b, secret: secret, handle: handle, now: time.Now}
}

// source is this carrier's identity to the endpoint: a stable,
// protocol-valid host derived from relay, body and owner. It carries no
// authority beyond naming the creator of requests submitted here.
func (c *Carrier) source(eventID string) core.Source {
	sum := sha256.Sum256([]byte("amq-remote/buzz/host\x00" + c.binding.RelayHost + "\x00" + c.binding.Body + "\x00" + c.binding.Owner))
	return core.Source{
		Host: "buzz-" + hex.EncodeToString(sum[:8]),
		Origin: map[string]string{
			"carrier": "buzz",
			"body":    c.binding.Body,
			"channel": c.binding.Channel,
			"event":   eventID,
		},
	}
}

// KindReaction is a NIP-25 reaction.
const KindReaction = 7

// cancelReaction is the owner's cancel gesture on a result row.
const cancelReaction = "❌"

// IngestReaction handles one verified kind 7 event: the owner reacting ❌
// on one of this edge's result rows cancels exactly that row's request
// (relay design §4, slice 5). The row is resolved through the persisted
// receipt mapping, not the reaction's own tags, and a reaction carries no h
// tag it has to match. Anything else is ignored; removing a reaction never
// undoes a cancel.
func (c *Carrier) IngestReaction(evt nostr.Event) error {
	if evt.Kind != KindReaction || evt.PubKey.Hex() != c.binding.Owner || strings.TrimSpace(evt.Content) != cancelReaction {
		return nil
	}
	target := ""
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "e" {
			target = t[1] // NIP-25: the last e tag is the reacted-to event
		}
	}
	if target == "" {
		return nil
	}
	ref, ok, err := c.ledger.RequestForRow(target)
	if err != nil || !ok {
		return err
	}
	now := c.now()
	created := time.Unix(int64(evt.CreatedAt), 0)
	if created.After(now.Add(maxFutureSkew)) || now.Sub(created) > MutationWindow {
		return nil // a stale cancel gesture never executes
	}
	cmd, _ := json.Marshal(map[string]string{"ref": ref})
	if _, _, err := c.ledger.Claim(Claim{EventID: evt.ID.Hex(), Owner: c.binding.Owner, Channel: c.binding.Channel, Op: OpCancel, Target: c.binding.Target, CreatedAt: int64(evt.CreatedAt), Command: cmd}); err != nil {
		return err
	}
	return c.statusOrCancel(evt, OpCancel, ref)
}

// Ingest handles one verified owner event from the subscription. The claim
// is persisted before the endpoint sees the command, and a redelivered
// event replays its stored claim, so one signed event is at most one
// request. Direct answers (inspect, status, cancel, unsupported) are
// prepared in the outbox keyed by the event; the submit's result row
// follows through Publish.
func (c *Carrier) Ingest(evt nostr.Event) error {
	n, err := Normalize(evt, c.binding, c.now())
	if errors.Is(err, ErrNotForUs) || errors.Is(err, ErrStale) {
		return nil // not ours, or too old to act on: no reply flood on replay
	}
	if err != nil {
		return c.answer(evt, err.Error())
	}
	claim := Claim{
		EventID: evt.ID.Hex(), Owner: c.binding.Owner, Channel: c.binding.Channel,
		Op: n.Op, RequestID: n.RequestID, Target: c.binding.Target, CreatedAt: int64(evt.CreatedAt),
	}
	if !n.NotAfter.IsZero() {
		claim.NotAfter = protocol.FormatTime(n.NotAfter)
	}
	if n.Op == OpSubmit {
		// The epoch is fixed at first sight and stored in the claim, so a
		// later import never retargets a new epoch.
		s, err := c.inspect()
		if err != nil {
			return c.answer(evt, "cannot reach the shared session: "+err.Error())
		}
		claim.Epoch = s.Epoch
	}
	cmdText := map[string]string{"text": n.Text, "ref": n.Ref}
	claim.Command, _ = json.Marshal(cmdText)
	claim, _, err = c.ledger.Claim(claim)
	if err != nil {
		return err
	}
	switch claim.Op {
	case OpSubmit:
		return c.submit(evt, claim, n.Text)
	case OpInspect:
		s, err := c.inspect()
		if err != nil {
			return c.answer(evt, "inspect failed: "+err.Error())
		}
		return c.answer(evt, sessionText(s))
	case OpStatus, OpCancel:
		return c.statusOrCancel(evt, claim.Op, n.Ref)
	default:
		return c.answer(evt, "unsupported command; use /inspect, /status <ref> or /cancel <ref>, or plain text to submit")
	}
}

func (c *Carrier) inspect() (protocol.Session, error) {
	out, err := c.handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: c.binding.Target}, c.source(""))
	if err != nil {
		return protocol.Session{}, err
	}
	s, ok := out.(protocol.Session)
	if !ok {
		return protocol.Session{}, fmt.Errorf("unexpected inspect reply %T", out)
	}
	return s, nil
}

func (c *Carrier) submit(evt nostr.Event, claim Claim, text string) error {
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: claim.RequestID, TargetID: claim.Target, Epoch: claim.Epoch, NotAfter: claim.NotAfter,
		Input: &protocol.SubmitInput{
			Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn,
			MinEvidence: string(protocol.EvidenceAdmitted),
		},
	}
	out, err := c.handle(cmd, c.source(claim.EventID))
	if err != nil {
		return c.answer(evt, "submit refused: "+err.Error())
	}
	reply, ok := out.(protocol.Reply)
	if !ok {
		return fmt.Errorf("unexpected submit reply %T", out)
	}
	// Record ownership now, before any row: /status and /cancel may only
	// address requests this edge submitted for this body.
	if _, owned, err := c.ledger.ReceiptFor(reply.Snapshot.RequestRef); err != nil {
		return err
	} else if !owned {
		if err := c.ledger.PutReceipt(Receipt{RequestRef: reply.Snapshot.RequestRef}); err != nil {
			return err
		}
	}
	return c.Publish(reply.Snapshot, c.source(claim.EventID).Origin)
}

func (c *Carrier) statusOrCancel(evt nostr.Event, op, ref string) error {
	if _, owned, err := c.ledger.ReceiptFor(ref); err != nil {
		return err
	} else if !owned {
		return c.answer(evt, "unknown request "+ref+" for this session")
	}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	if op == OpCancel {
		cmd.Op = protocol.OpRequestCancel
	}
	out, err := c.handle(cmd, c.source(evt.ID.Hex()))
	if err != nil {
		return c.answer(evt, op+" failed: "+err.Error())
	}
	reply, ok := out.(protocol.Reply)
	if !ok {
		return fmt.Errorf("unexpected %s reply %T", op, out)
	}
	return c.answer(evt, snapshotText(reply.Snapshot, reply.Outcome.Message))
}

// answer prepares a direct reply to one ingress event, once.
func (c *Carrier) answer(to nostr.Event, text string) error {
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(c.now().Unix()),
		Kind:      KindDM,
		Tags:      c.rowTags(),
		Content:   text,
	}
	return c.prepare("direct/"+to.ID.Hex(), evt)
}

// Publish is this carrier's core.Publisher for records whose origin is
// this body. The first revision creates the request's one row (kind 9);
// later revisions are kind 40003 edits of that row, each dated strictly
// after the previous one. Returning nil means the output is durably owed
// in the outbox; Flush delivers it.
func (c *Carrier) Publish(snap protocol.Snapshot, origin map[string]string) error {
	if origin["carrier"] != "buzz" || origin["body"] != c.binding.Body {
		return fmt.Errorf("record %s is not this Buzz carrier's", snap.RequestRef)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rc, _, err := c.ledger.ReceiptFor(snap.RequestRef)
	if err != nil {
		return err
	}
	if rc.RequestRef == "" {
		rc.RequestRef = snap.RequestRef
	}
	if int(snap.Revision) <= rc.Revision && rc.RootEventID != "" {
		return nil // already prepared this or a newer revision
	}
	text := snapshotText(snap, "")
	now := c.now().Unix()
	key := fmt.Sprintf("row/%s/%08d", snap.RequestRef, snap.Revision)
	var evt nostr.Event
	if rc.RootEventID == "" {
		evt = nostr.Event{CreatedAt: nostr.Timestamp(now), Kind: KindDM, Tags: c.rowTags(), Content: text}
	} else {
		if now <= rc.LastEditAt {
			return ErrClockBehind
		}
		evt = nostr.Event{CreatedAt: nostr.Timestamp(now), Kind: KindEdit, Tags: c.editTags(rc.RootEventID), Content: text}
	}
	if err := evt.Sign(c.secret); err != nil {
		return err
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	stored, err := c.ledger.Prepare(key, raw)
	if err != nil {
		return err
	}
	var prepared nostr.Event
	if err := json.Unmarshal(stored.Event, &prepared); err != nil {
		return err
	}
	if rc.RootEventID == "" {
		rc.RootEventID = prepared.ID.Hex()
	}
	rc.LastEditAt, rc.Revision = int64(prepared.CreatedAt), int(snap.Revision)
	return c.ledger.PutReceipt(rc)
}

func (c *Carrier) prepare(key string, evt nostr.Event) error {
	if err := evt.Sign(c.secret); err != nil {
		return err
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = c.ledger.Prepare(key, raw)
	return err
}

// Flush sends every owed output in order and records acceptance on the
// relay's matching OK. An explicit rejection or an unknown result leaves
// the output owed; the same stored bytes are sent again next time.
func (c *Carrier) Flush(ctx context.Context, pub Publisher) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, err := c.ledger.Pending()
	if err != nil {
		return err
	}
	for _, o := range pending {
		var evt nostr.Event
		if err := json.Unmarshal(o.Event, &evt); err != nil {
			return fmt.Errorf("outbox %s: %w", o.Key, err)
		}
		if err := pub(ctx, evt); err != nil {
			return fmt.Errorf("publish %s: %w", o.Key, err)
		}
		if err := c.ledger.MarkAccepted(o.Key); err != nil {
			return err
		}
	}
	return nil
}

// rowTags and editTags are the only place row tags are built (relay design
// B2/B3): a row lives in the owner DM channel (h); an edit names its
// original row (e) in the same channel.
func (c *Carrier) rowTags() nostr.Tags {
	return nostr.Tags{{"h", c.binding.Channel}}
}

func (c *Carrier) editTags(rootID string) nostr.Tags {
	return nostr.Tags{{"h", c.binding.Channel}, {"e", rootID}}
}

func sessionText(s protocol.Session) string {
	caps := []string{}
	if s.Capabilities.Submit {
		caps = append(caps, "submit")
	}
	if s.Capabilities.CancelRequest {
		caps = append(caps, "cancel")
	}
	return fmt.Sprintf("%s (%s): %s, %s; can %s; observed %s", s.TargetID, s.Harness, s.Attachment, s.Status, strings.Join(caps, ", "), s.ObservedAt)
}

// snapshotText renders one revision of a request for its row: ref, state,
// code and reason, then the result, shortened with an explicit marker.
func snapshotText(s protocol.Snapshot, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", s.RequestRef, s.State)
	if s.Code != "" {
		fmt.Fprintf(&b, " (%s)", s.Code)
	}
	if reason != "" {
		fmt.Fprintf(&b, ": %s", reason)
	}
	if s.Result != nil && s.Result.Text != "" {
		text := s.Result.Text
		if len(text) > maxRowText || s.Result.Truncated {
			for len(text) > maxRowText || !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			text += "\n[shortened; run amq-remote status " + s.RequestRef + " for the full result]"
		}
		b.WriteString("\n\n" + text)
	}
	return b.String()
}
