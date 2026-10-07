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
	"unicode"
	"unicode/utf8"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
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

// ErrNoGrant means the enrolled generation holds no owner grant admitting
// an event of this kind at this time. Buzz does not enforce NIP-OA kind
// grants, so AMQ refuses to sign locally (slice 4 contract §4).
var ErrNoGrant = errors.New("no enrolled owner grant for this event")

// Grant returns the enrolled owner tag that admits kind at createdAt, or an
// error when none does.
type Grant func(kind uint16, createdAt time.Time) (bodykey.AuthTag, error)

// Handler is the endpoint's command entry point (core.Endpoint.Handle).
type Handler func(cmd *protocol.Command, src core.Source) (any, error)

// Publisher sends one signed event and returns nil only on the relay's
// matching positive OK (internal/relay Conn.Publish).
type Publisher func(ctx context.Context, evt nostr.Event) error

// Carrier is one shared session's owner-DM edge: it turns verified owner
// events into endpoint commands and endpoint snapshots into one editable
// result row per request.
type Carrier struct {
	Signer
	ledger  *Ledger
	binding Binding
	handle  Handler
	// identity returns the target's attached native session id (the
	// endpoint's in-process NativeSessionID), "" when unproven.
	identity func(target string) string
	now      func() time.Time

	mu sync.Mutex // serializes row preparation (edit dating) and flush
}

// NewCarrier builds the edge for one binding. secret is the body key and
// grant looks up the enrolled owner grants every signed event needs.
func NewCarrier(ledger *Ledger, b Binding, secret [32]byte, grant Grant, identity func(target string) string, handle Handler) *Carrier {
	return &Carrier{Signer: Signer{secret: secret, grant: grant}, ledger: ledger, binding: b, handle: handle, identity: identity, now: time.Now}
}

// share is this carrier's full binding, recorded on every output.
func (c *Carrier) share() ShareBinding {
	return ShareBinding{
		Relay: c.binding.RelayHost, Owner: c.binding.Owner, Body: c.binding.Body,
		Channel: c.binding.Channel, Target: c.binding.Target, NativeSession: c.binding.NativeSession,
	}
}

// Signer signs the body's events under the owner's enrolled grants.
type Signer struct {
	secret [32]byte
	grant  Grant
}

// NewSigner returns a signer for the body key secret and its grants.
func NewSigner(secret [32]byte, grant Grant) Signer { return Signer{secret: secret, grant: grant} }

// sign enforces the owner's grant for the event's kind at its created_at,
// attaches that grant as the one NIP-OA provenance tag, and signs. An event
// no grant admits is never signed.
func (s Signer) sign(evt *nostr.Event) error {
	tag, err := s.grant(uint16(evt.Kind), time.Unix(int64(evt.CreatedAt), 0))
	if err != nil {
		return fmt.Errorf("%w: kind %d: %v", ErrNoGrant, evt.Kind, err)
	}
	evt.Tags = append(evt.Tags, nostr.Tag{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	return evt.Sign(s.secret)
}

// source is this carrier's identity to the endpoint: a stable,
// protocol-valid host derived from relay, body and owner. It carries no
// authority beyond naming the creator of requests submitted here.
func (c *Carrier) source(eventID, root string) core.Source {
	sum := sha256.Sum256([]byte("amq-remote/buzz/host\x00" + c.binding.RelayHost + "\x00" + c.binding.Body + "\x00" + c.binding.Owner))
	src := core.Source{
		Host: "buzz-" + hex.EncodeToString(sum[:8]),
		Origin: map[string]string{
			"carrier": "buzz",
			"body":    c.binding.Body,
			"channel": c.binding.Channel,
			"event":   eventID,
		},
	}
	if root != "" {
		src.Origin["root"] = root // the input's thread root, derived from the signed event
	}
	return src
}

// sourceFor is the source of a request submitted by evt. A mention-channel
// input is marked, so its result row carries no reference into the channel
// it came from.
func (c *Carrier) sourceFor(evt nostr.Event) core.Source {
	if tagValue(evt, "h") == c.binding.Channel {
		return c.source(evt.ID.Hex(), threadRoot(evt))
	}
	src := c.source(evt.ID.Hex(), "")
	src.Origin["entry"] = "mention"
	return src
}

// replyTags are a direct answer's tags: a reply in the DM thread, or a
// standalone DM row for mention-channel input.
func (c *Carrier) replyTags(to nostr.Event) nostr.Tags {
	if tagValue(to, "h") != c.binding.Channel {
		return c.rowTags("", "")
	}
	return c.rowTags(to.ID.Hex(), threadRoot(to))
}

// originTags are a result row's tags from its record's origin.
func (c *Carrier) originTags(origin map[string]string) nostr.Tags {
	if origin["entry"] == "mention" {
		return c.rowTags("", "")
	}
	return c.rowTags(origin["event"], origin["root"])
}

// threadRoot is the NIP-10 root an owner event replies within, if any.
func threadRoot(evt nostr.Event) string {
	for _, t := range evt.Tags {
		if len(t) >= 4 && t[0] == "e" && t[3] == "root" && validHexID(t[1]) {
			return t[1]
		}
	}
	return ""
}

// KindReaction is a NIP-25 reaction.
const KindReaction = 7

// cancelReaction is the owner's cancel gesture on a result row.
const cancelReaction = "❌"

// IngestReaction handles one verified kind 7 event from the owner. On an
// approval message this edge posted, ✅ approves and ❌ rejects exactly that
// interaction (answerApproval). On one of this edge's result rows, ❌
// cancels exactly that row's request (relay design §4, slice 5); the row is
// resolved through the persisted receipt mapping, not the reaction's own
// tags, and a reaction carries no h tag it has to match. Anything else is
// ignored; removing a reaction never undoes a cancel or an answer.
func (c *Carrier) IngestReaction(evt nostr.Event) error {
	if evt.Kind != KindReaction || evt.PubKey.Hex() != c.binding.Owner {
		return nil
	}
	target := lastETag(evt) // NIP-25: the last e tag is the reacted-to event
	gesture := strings.TrimSpace(evt.Content)
	if target == "" || gesture != approveReaction && !rejectReaction(gesture) {
		return nil
	}
	appr, isApproval, err := c.ledger.ApprovalFor(target)
	if err != nil {
		return err
	}
	if isApproval {
		return c.answerApproval(evt, target, appr, gesture)
	}
	if gesture != cancelReaction {
		return nil
	}
	ref, ok, err := c.ledger.RequestForRow(target)
	if err != nil || !ok {
		return err
	}
	cmd, _ := json.Marshal(map[string]string{"ref": ref})
	notAfter, ok, err := c.claimReaction(evt, OpCancel, "", cmd)
	if err != nil || !ok {
		return err
	}
	return c.statusOrCancel(evt, OpCancel, ref, notAfter)
}

// claimReaction admits one owner reaction as one decision: it must be
// fresh, the surface granted, the event not yet settled, the shared
// session unchanged, and the claim made under this binding. ok is false
// when the reaction must be ignored. notAfter is the reaction's signed
// deadline.
func (c *Carrier) claimReaction(evt nostr.Event, op, epoch string, cmd json.RawMessage) (time.Time, bool, error) {
	now := c.now()
	created := time.Unix(int64(evt.CreatedAt), 0)
	notAfter := created.Add(MutationWindow)
	if created.After(now.Add(maxFutureSkew)) || now.After(notAfter) {
		return notAfter, false, nil // a stale gesture never executes
	}
	if err := c.eligible(now); err != nil {
		return notAfter, false, err
	}
	if _, settled, err := c.ledger.Settled(evt.ID.Hex()); err != nil || settled {
		return notAfter, false, err
	}
	if err := c.fence(); err != nil {
		return notAfter, false, nil // a replacement native session is never acted on for a reaction
	}
	claim, _, err := c.ledger.Claim(c.claimFor(evt, op, "", epoch, notAfter, cmd))
	if err != nil || !c.owns(claim) {
		return notAfter, false, err
	}
	return notAfter, true, nil
}

// Ingest handles one verified owner event from the subscription. The claim
// is persisted before the endpoint sees the command, and the command's
// settlement after, so one signed event is at most one decision: a
// redelivered settled event is not sent to the endpoint again. Direct
// answers (inspect, status, cancel, unsupported) are prepared in the outbox
// keyed by the event; the submit's result row follows through Publish.
func (c *Carrier) Ingest(evt nostr.Event) error {
	return c.ingest(evt, Normalize)
}

// IngestMention handles one verified owner event from a mention channel:
// the same claim and submit path as a DM, with every answer and result row
// going to the DM channel (slice 5).
func (c *Carrier) IngestMention(evt nostr.Event) error {
	return c.ingest(evt, NormalizeMention)
}

func (c *Carrier) ingest(evt nostr.Event, normalize func(nostr.Event, Binding, time.Time) (Normalized, error)) error {
	now := c.now()
	n, err := normalize(evt, c.binding, now)
	if errors.Is(err, ErrNotForUs) || errors.Is(err, ErrStale) {
		return nil // not ours, or too old to act on: no reply flood on replay
	}
	// The whole enabled surface must be granted before anything is admitted:
	// a share whose owner never enabled buzz-dm must not run work it cannot
	// answer (codex #866 r1 #3).
	if gerr := c.eligible(now); gerr != nil {
		return gerr
	}
	if _, settled, serr := c.ledger.Settled(evt.ID.Hex()); serr != nil || settled {
		return serr
	}
	if err != nil {
		return c.answer(evt, protocol.InertInline(err.Error()))
	}
	// Every command, not only submit, runs only against the approved native
	// session: /inspect of a replacement session is never answered (codex
	// #866 r2 #7).
	if err := c.fence(); err != nil {
		return c.settleAnswer(evt, Settlement{Op: n.Op, State: "refused"}, protocol.InertInline(err.Error()))
	}
	claim := c.claimFor(evt, n.Op, n.RequestID, "", n.NotAfter, nil)
	if n.Op == OpSubmit {
		// The epoch is fixed at first sight and stored in the claim, so a
		// later import never retargets a new epoch.
		s, err := c.inspect()
		if err != nil {
			return c.answer(evt, "cannot reach the shared session: "+protocol.InertInline(err.Error()))
		}
		claim.Epoch = s.Epoch
		// The epoch is now fixed. The approved session must still be the
		// attached one: a switch before the inspect is refused here, and a
		// switch after it fails the fixed epoch at dispatch.
		if err := c.fence(); err != nil {
			return c.settleAnswer(evt, Settlement{Op: n.Op, State: "refused"}, protocol.InertInline(err.Error()))
		}
	}
	claim.Command, _ = json.Marshal(map[string]string{"text": n.Text, "ref": n.Ref})
	claim, created, err := c.ledger.Claim(claim)
	if err != nil {
		return err
	}
	if !c.owns(claim) {
		return nil // claimed under another share binding: never acted on here
	}
	switch claim.Op {
	case OpSubmit:
		return c.submit(evt, claim, created, n.Text)
	case OpInspect:
		s, err := c.inspect()
		if err != nil {
			return c.settleAnswer(evt, Settlement{Op: claim.Op}, "inspect failed: "+protocol.InertInline(err.Error()))
		}
		return c.settleAnswer(evt, Settlement{Op: claim.Op}, sessionText(s))
	case OpStatus, OpCancel:
		return c.statusOrCancel(evt, claim.Op, n.Ref, n.NotAfter)
	default:
		return c.settleAnswer(evt, Settlement{Op: claim.Op}, "unsupported command; use /inspect, /status <ref> or /cancel <ref>, or plain text to submit")
	}
}

// claimFor is the claim of evt under this carrier's binding.
func (c *Carrier) claimFor(evt nostr.Event, op, requestID, epoch string, notAfter time.Time, cmd json.RawMessage) Claim {
	cl := Claim{
		EventID: evt.ID.Hex(), Owner: c.binding.Owner, Body: c.binding.Body, Relay: c.binding.RelayHost,
		Channel: tagValue(evt, "h"), Op: op, RequestID: requestID, Target: c.binding.Target, Epoch: epoch,
		CreatedAt: int64(evt.CreatedAt), Command: cmd,
		DMChannel: c.binding.Channel, NativeSession: c.binding.NativeSession,
	}
	if cl.Channel == "" {
		cl.Channel = c.binding.Channel // a reaction carries no h
	}
	if !notAfter.IsZero() {
		cl.NotAfter = protocol.FormatTime(notAfter)
	}
	return cl
}

// owns reports whether a stored claim was made under this carrier's
// binding. A claim from an earlier binding (another channel, body, relay or
// target) is never replayed here (codex #866 r1 #6).
func (c *Carrier) owns(cl Claim) bool {
	return cl.Owner == c.binding.Owner && cl.Body == c.binding.Body && cl.Relay == c.binding.RelayHost &&
		cl.Target == c.binding.Target && cl.DMChannel == c.binding.Channel && cl.NativeSession == c.binding.NativeSession &&
		(cl.Channel == c.binding.Channel || c.binding.Mentions[cl.Channel])
}

// ownsReceipt is owns for a request's receipt.
func (c *Carrier) ownsReceipt(r Receipt) bool {
	return r.Owner == c.binding.Owner && r.Body == c.binding.Body && r.Relay == c.binding.RelayHost &&
		r.Target == c.binding.Target && r.Channel == c.binding.Channel && r.NativeSession == c.binding.NativeSession
}

// eligible reports whether the enrolled generation grants every kind the
// DM surface signs, now.
func (c *Carrier) eligible(now time.Time) error {
	for _, kind := range []uint16{KindDM, KindEdit} {
		if _, err := c.grant(kind, now); err != nil {
			return fmt.Errorf("%w: kind %d: %v", ErrNoGrant, kind, err)
		}
	}
	return nil
}

// ErrNotShared is a target whose attached native session is not the one the
// operator approved for sharing, or that cannot prove its identity.
var ErrNotShared = errors.New("the attached session is not the one approved for sharing")

// fence checks the target's attached native session against the approved
// identity, through the endpoint's in-process accessor (never the session
// wire schema, codex #866 r2 #6).
func (c *Carrier) fence() error {
	if c.binding.NativeSession == "" || c.identity == nil || c.identity(c.binding.Target) != c.binding.NativeSession {
		return ErrNotShared
	}
	return nil
}

// Shared reports whether the target's attached native session is the one
// the operator approved for sharing.
func (c *Carrier) Shared() error { return c.fence() }

func (c *Carrier) inspect() (protocol.Session, error) {
	out, err := c.handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: c.binding.Target}, c.source("", ""))
	if err != nil {
		return protocol.Session{}, err
	}
	s, ok := out.(protocol.Session)
	if !ok {
		return protocol.Session{}, fmt.Errorf("unexpected inspect reply %T", out)
	}
	return s, nil
}

// submit dispatches a claimed submit. The request's ref is deterministic
// (protocol.EncodeRef of source host, target and request id), so its
// receipt is written before dispatch and a result is never stranded without
// one (codex #866 r2 #2). A claim that already existed but has no
// settlement is an interrupted import: the core's own decision is read back
// first and only a request the core never saw is submitted, so an
// interrupted busy rejection never turns into execution (codex #866 r2 #1).
// The settlement is recorded only after the row is prepared.
func (c *Carrier) submit(evt nostr.Event, claim Claim, created bool, text string) error {
	src := c.sourceFor(evt)
	ref := protocol.EncodeRef(src.Host, claim.Target, claim.RequestID)
	rc, owned, err := c.ledger.ReceiptFor(ref)
	if err != nil {
		return err
	}
	if !owned {
		if err := c.ledger.PutReceipt(c.receiptFor(ref, claim)); err != nil {
			return err
		}
	}
	var reply protocol.Reply
	recovered := false
	if !created {
		// A decision already shown to the owner (a direct answer, or the
		// request's result row) settles the command even when the core
		// stored nothing, as for a refusal before admission: it is never
		// resubmitted (codex #866 r3 #1).
		_, answered, err := c.ledger.Prepared("direct/" + evt.ID.Hex())
		if err != nil {
			return err
		}
		// The root row is committed before its id reaches the receipt, so the
		// prepared root obligation itself is checked, and the receipt is
		// restored from it (codex #866 r4).
		if rc.RootEventID == "" {
			if root, ok, err := c.ledger.Prepared(rootKey(ref)); err != nil {
				return err
			} else if ok && root.Binding == c.share() {
				var evt nostr.Event
				if err := json.Unmarshal(root.Event, &evt); err != nil {
					return fmt.Errorf("outbox %s: %w", root.Key, err)
				}
				rc = c.receiptFor(ref, claim)
				rc.RootEventID, rc.LastEditAt, rc.Revision = evt.ID.Hex(), int64(evt.CreatedAt), root.Revision
				if err := c.ledger.PutReceipt(rc); err != nil {
					return err
				}
			}
		}
		if answered || rc.RootEventID != "" {
			_, err := c.ledger.Settle(evt.ID.Hex(), Settlement{Op: claim.Op, RequestRef: ref, State: "decided"})
			return err
		}
		out, err := c.handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, src)
		var refusal *protocol.Refusal
		switch {
		case err == nil:
			r, ok := out.(protocol.Reply)
			if !ok {
				return fmt.Errorf("unexpected get reply %T", out)
			}
			reply, recovered = r, true
		case errors.As(err, &refusal) && refusal.Code == protocol.CodeNotFound:
			// the core never saw it: submit below
		default:
			return err // unknown: stay unsettled and retry on the next delivery
		}
	}
	if !recovered {
		cmd := &protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
			RequestID: claim.RequestID, TargetID: claim.Target, Epoch: claim.Epoch, NotAfter: claim.NotAfter,
			Input: &protocol.SubmitInput{
				Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn,
				MinEvidence: c.minEvidence(),
			},
		}
		out, err := c.handle(cmd, src)
		if err != nil {
			return c.settleAnswer(evt, Settlement{Op: claim.Op, RequestRef: ref, State: "refused"}, "submit refused: "+protocol.InertInline(err.Error()))
		}
		r, ok := out.(protocol.Reply)
		if !ok {
			return fmt.Errorf("unexpected submit reply %T", out)
		}
		reply = r
	}
	if reply.Snapshot.RequestRef != ref {
		return fmt.Errorf("endpoint answered request %s with ref %s, want %s", claim.RequestID, reply.Snapshot.RequestRef, ref)
	}
	if err := c.Publish(reply.Snapshot, src.Origin); err != nil {
		return err
	}
	_, err = c.ledger.Settle(evt.ID.Hex(), Settlement{Op: claim.Op, RequestRef: ref, State: string(reply.Snapshot.State)})
	return err
}

// minEvidence is the share's submit evidence floor, admitted by default.
func (c *Carrier) minEvidence() string {
	if c.binding.MinEvidence == protocol.EvidenceSubmitted {
		return protocol.EvidenceSubmitted
	}
	return protocol.EvidenceAdmitted
}

// Notes for a share at submitted evidence, on a request that has not ended.
// Only a running request has the delivery evidence; in any other state the
// note states the policy and claims nothing about this request.
const (
	submittedRunningNote = "\n\nThe request reached the session; its start is not proven (this share accepts submitted evidence)."
	submittedPolicyNote  = "\n\nThis share accepts submitted evidence: a request counts once the session has it, and its start is not proven."
)

// receiptFor is a new request's receipt under this binding.
func (c *Carrier) receiptFor(ref string, claim Claim) Receipt {
	return Receipt{
		RequestRef: ref, Owner: c.binding.Owner, Body: c.binding.Body, Relay: c.binding.RelayHost,
		Channel: c.binding.Channel, Target: claim.Target, Epoch: claim.Epoch, NativeSession: c.binding.NativeSession,
	}
}

// statusOrCancel answers /status or performs /cancel (or a ❌) for a request
// this share submitted. Cancel carries the request's stored target and
// epoch and the owner command's signed deadline (codex #866 r1 #1).
func (c *Carrier) statusOrCancel(evt nostr.Event, op, ref string, notAfter time.Time) error {
	rc, owned, err := c.ledger.ReceiptFor(ref)
	if err != nil {
		return err
	}
	if !owned || !c.ownsReceipt(rc) {
		return c.settleAnswer(evt, Settlement{Op: op}, "unknown request "+ref+" for this session")
	}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	if op == OpCancel {
		cmd.Op = protocol.OpRequestCancel
		cmd.TargetID, cmd.Epoch, cmd.NotAfter = rc.Target, rc.Epoch, protocol.FormatTime(notAfter)
	}
	out, err := c.handle(cmd, c.source(evt.ID.Hex(), ""))
	if err != nil {
		return c.settleAnswer(evt, Settlement{Op: op, RequestRef: ref, State: "refused"}, op+" failed: "+protocol.InertInline(err.Error()))
	}
	reply, ok := out.(protocol.Reply)
	if !ok {
		return fmt.Errorf("unexpected %s reply %T", op, out)
	}
	return c.settleAnswer(evt, Settlement{Op: op, RequestRef: ref, State: string(reply.Snapshot.State)}, snapshotText(reply.Snapshot, reply.Outcome.Message))
}

// settleAnswer prepares the command's answer, then records its settlement:
// a failed answer leaves the command unsettled, so its redelivery answers it
// (codex #866 r2 #3). The answer is keyed by the event, so a retry returns
// the stored one.
func (c *Carrier) settleAnswer(evt nostr.Event, st Settlement, text string) error {
	if err := c.answer(evt, text); err != nil {
		return err
	}
	_, err := c.ledger.Settle(evt.ID.Hex(), st)
	return err
}

// answer prepares a direct reply to one ingress event, once.
func (c *Carrier) answer(to nostr.Event, text string) error {
	return c.reply(to, c.replyTags(to), text)
}

// reply prepares the one direct reply owed for ingress event to, with tags.
func (c *Carrier) reply(to nostr.Event, tags nostr.Tags, text string) error {
	evt := nostr.Event{CreatedAt: nostr.Timestamp(c.now().Unix()), Kind: KindDM, Tags: tags, Content: text}
	return c.prepare("direct/"+to.ID.Hex(), evt)
}

// Publish is this carrier's core.Publisher for records whose origin is
// this body and DM channel. A request has one root row (kind 9) under a
// revision-independent key, so a crash after preparing it is recovered,
// never duplicated; later revisions are kind 40003 edits of that row, each
// dated strictly after the previous one (codex #866 r1 #5). Returning nil
// means the output is durably owed in the outbox; Flush delivers it.
func (c *Carrier) Publish(snap protocol.Snapshot, origin map[string]string) error {
	if origin["carrier"] != "buzz" || origin["body"] != c.binding.Body || origin["channel"] != c.binding.Channel {
		return fmt.Errorf("record %s is not this Buzz carrier's", snap.RequestRef)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rc, owned, err := c.ledger.ReceiptFor(snap.RequestRef)
	if err != nil {
		return err
	}
	if !owned || !c.ownsReceipt(rc) {
		return fmt.Errorf("record %s has no receipt under this share binding", snap.RequestRef)
	}
	text := snapshotText(snap, "")
	if c.minEvidence() == protocol.EvidenceSubmitted && !snap.State.Terminal() {
		if snap.State == protocol.StateRunning {
			text += submittedRunningNote
		} else {
			text += submittedPolicyNote
		}
	}
	if rc.RootEventID == "" {
		evt := nostr.Event{CreatedAt: nostr.Timestamp(c.now().Unix()), Kind: KindDM, Tags: c.originTags(origin), Content: text}
		stored, err := c.prepareRow(rootKey(snap.RequestRef), evt, int(snap.Revision))
		if err != nil {
			return err
		}
		rc.RootEventID, rc.LastEditAt, rc.Revision = stored.ID.Hex(), int64(stored.CreatedAt), stored.revision
		if err := c.ledger.PutReceipt(rc); err != nil {
			return err
		}
	} else if err := c.ledger.ensureRowMap(rc); err != nil {
		return err
	}
	if err := c.prepareApprovals(snap, rc, origin); err != nil {
		return err
	}
	if int(snap.Revision) <= rc.Revision {
		return nil // this or a newer revision is already prepared
	}
	now := c.now().Unix()
	if now <= rc.LastEditAt {
		return ErrClockBehind
	}
	// Reserve the edit's second before preparing it: after a crash between
	// the two, the next edit is still dated strictly later than any edit
	// already prepared (codex #866 r2 #5).
	rc.LastEditAt = now
	if err := c.ledger.PutReceipt(rc); err != nil {
		return err
	}
	evt := nostr.Event{CreatedAt: nostr.Timestamp(now), Kind: KindEdit, Tags: c.editTags(rc.RootEventID), Content: text}
	stored, err := c.prepareRow(fmt.Sprintf("row/%s/%08d", snap.RequestRef, snap.Revision), evt, int(snap.Revision))
	if err != nil {
		return err
	}
	editPrepared()
	if int64(stored.CreatedAt) > rc.LastEditAt {
		rc.LastEditAt = int64(stored.CreatedAt)
	}
	rc.Revision = stored.revision
	return c.ledger.PutReceipt(rc)
}

// Owner reactions on an approval message. Approve runs a command, so it
// takes only an explicit ✅, never the "+" that a plain like sends; reject
// fails safe and takes any of the usual refusals.
const approveReaction = "✅"

func rejectReaction(gesture string) bool {
	return gesture == "❌" || gesture == "👎" || gesture == "-"
}

// approvalKey is the outbox key of the message that shows one interaction.
// It sorts after the request's row events, so Flush sends the row first.
func approvalKey(ref, interactionID string) string {
	return fmt.Sprintf("row/%s/approval/%s", ref, interactionID)
}

// prepareApprovals posts one message for a pending approval that the owner
// can answer, edits it when whether a remote answer can apply changes, and
// edits it once with the outcome when the record resolves it. Every edit is
// keyed per interaction and revision, so a republished revision changes
// nothing.
func (c *Carrier) prepareApprovals(snap protocol.Snapshot, rc Receipt, origin map[string]string) error {
	if err := c.refreshApproval(snap, rc); err != nil {
		return err
	}
	// An older revision replayed after a newer one never posts its approval:
	// that interaction may be resolved already.
	if in := snap.Interaction; in != nil && in.RemoteAnswer && in.Kind == "approval" && int(snap.Revision) >= rc.Revision {
		appr := Approval{RequestRef: snap.RequestRef, InteractionID: in.InteractionID, Target: rc.Target, Epoch: snap.Epoch,
			Prompt: in.Prompt, ApproveOption: in.ApproveOption, RejectOption: in.RejectOption}
		evt := nostr.Event{CreatedAt: nostr.Timestamp(c.now().Unix()), Kind: KindDM, Tags: c.originTags(origin), Content: approvalText(appr, "")}
		stored, err := c.prepareRow(approvalKey(snap.RequestRef, in.InteractionID), evt, 0)
		if err != nil {
			return err
		}
		if err := c.ledger.PutApproval(stored.ID.Hex(), appr); err != nil {
			return err
		}
	}
	for _, r := range snap.Resolved {
		key := approvalKey(snap.RequestRef, r.InteractionID)
		if _, done, err := c.ledger.Prepared(key + "/outcome"); err != nil || done {
			if err != nil {
				return err
			}
			continue
		}
		posted, ok, err := c.ledger.Prepared(key)
		if err != nil {
			return err
		}
		if !ok {
			continue // never shown here: nothing to update
		}
		var msg nostr.Event
		if err := json.Unmarshal(posted.Event, &msg); err != nil {
			return err
		}
		appr, ok, err := c.ledger.ApprovalFor(msg.ID.Hex())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if appr.Pending != nil {
			if appr, err = c.finishApprovalEdit(msg, appr); err != nil {
				return err
			}
		}
		now := c.now().Unix()
		if now <= int64(msg.CreatedAt) || now <= appr.EditedAt {
			return ErrClockBehind
		}
		edit := nostr.Event{CreatedAt: nostr.Timestamp(now), Kind: KindEdit, Tags: c.editTags(msg.ID.Hex()), Content: approvalText(appr, outcomeText(r, appr))}
		if err := c.prepare(key+"/outcome", edit); err != nil {
			return err
		}
	}
	return nil
}

// refreshApproval edits a posted approval whose answerability changed: the
// pending interaction stopped, or started again, taking a remote answer, or
// offers other options. The message and the reaction mapping change as one
// recoverable step: the intended edit is recorded on the mapping first, then
// prepared, then the mapping takes the new view. An edit a crash left
// unfinished is finished before anything is compared, so the message and the
// mapping always end in agreement.
func (c *Carrier) refreshApproval(snap protocol.Snapshot, rc Receipt) error {
	in := snap.Interaction
	if in == nil || in.Kind != "approval" || int(snap.Revision) < rc.Revision {
		return nil
	}
	key := approvalKey(snap.RequestRef, in.InteractionID)
	posted, ok, err := c.ledger.Prepared(key)
	if err != nil || !ok {
		return err // never posted: nothing to edit
	}
	var msg nostr.Event
	if err := json.Unmarshal(posted.Event, &msg); err != nil {
		return err
	}
	appr, ok, err := c.ledger.ApprovalFor(msg.ID.Hex())
	if err != nil || !ok {
		return err
	}
	if appr.Pending != nil {
		if appr, err = c.finishApprovalEdit(msg, appr); err != nil {
			return err
		}
	}
	want := appr
	want.Disabled = !in.RemoteAnswer
	if in.RemoteAnswer {
		want.ApproveOption, want.RejectOption = in.ApproveOption, in.RejectOption
	}
	if want.Disabled == appr.Disabled && want.ApproveOption == appr.ApproveOption && want.RejectOption == appr.RejectOption {
		return nil
	}
	now := c.now().Unix()
	if now <= int64(msg.CreatedAt) || now <= appr.EditedAt {
		return ErrClockBehind
	}
	appr.Pending = &ApprovalEdit{View: want, Key: fmt.Sprintf("%s/edit/%08d", key, snap.Revision), At: now}
	if err := c.ledger.UpdateApproval(msg.ID.Hex(), appr); err != nil {
		return err
	}
	_, err = c.finishApprovalEdit(msg, appr)
	return err
}

// finishApprovalEdit prepares the edit recorded on appr, idempotently, and
// then gives the mapping that edit's view, dated by the stored event.
func (c *Carrier) finishApprovalEdit(msg nostr.Event, appr Approval) (Approval, error) {
	p := appr.Pending
	edit := nostr.Event{CreatedAt: nostr.Timestamp(p.At), Kind: KindEdit, Tags: c.editTags(msg.ID.Hex()), Content: approvalText(p.View, "")}
	if err := c.prepare(p.Key, edit); err != nil {
		return appr, err
	}
	stored, ok, err := c.ledger.Prepared(p.Key)
	if err != nil || !ok {
		return appr, fmt.Errorf("approval edit %s not prepared: %v", p.Key, err)
	}
	var evt nostr.Event
	if err := json.Unmarshal(stored.Event, &evt); err != nil {
		return appr, err
	}
	approvalEditPrepared()
	done := p.View
	done.Pending, done.EditedAt = nil, int64(evt.CreatedAt)
	return done, c.ledger.UpdateApproval(msg.ID.Hex(), done)
}

// approvalEditPrepared runs between preparing an approval edit and giving
// the mapping its view; tests replace it to crash there.
var approvalEditPrepared = func() {}

// approvalText is an approval message: what the harness asks to do and how
// to answer it, or, once resolved, the outcome instead of the instructions.
func approvalText(a Approval, outcome string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approval needed (%s):\n\n%s", a.RequestRef, boundPreview(a.Prompt))
	switch {
	case outcome != "":
		b.WriteString("\n\n" + outcome)
	case a.Disabled:
		b.WriteString("\n\nIt can no longer be answered from Buzz. Answer in the terminal.")
	case a.ApproveOption != "" && a.RejectOption != "":
		b.WriteString("\n\nReact ✅ to approve or ❌ to reject. The first answer, here or in the terminal, wins.")
	case a.ApproveOption != "":
		b.WriteString("\n\nReact ✅ to approve, or answer in the terminal. The first answer wins.")
	case a.RejectOption != "":
		b.WriteString("\n\nReact ❌ to reject, or answer in the terminal. The first answer wins.")
	default:
		b.WriteString("\n\nAnswer in the terminal.")
	}
	return b.String()
}

// outcomeText is the owner-facing line for one resolution.
func outcomeText(r protocol.Resolution, a Approval) string {
	switch r.Outcome {
	case protocol.ResolutionAnswered:
		// The harness applies the first answer it receives, so a terminal
		// answer given in the same moment can still be the one that applied.
		const race = " If the terminal answered at the same moment, its answer applied."
		switch r.Option {
		case a.ApproveOption:
			return "✅ Approve was sent from Buzz." + race
		case a.RejectOption:
			return "❌ Reject was sent from Buzz." + race
		}
		return fmt.Sprintf("Answer %q was sent from Buzz.", r.Option) + race
	case protocol.ResolutionRunEnded:
		return "The run ended before this was answered."
	case protocol.ResolutionDeliveryUnknown:
		return deliveryUnknownText
	}
	return "Closed outside Buzz: answered in the terminal, or the turn stopped."
}

// boundPreview shortens a prompt for display and removes control
// characters other than newline and tab, so the message shows what will run
// and nothing hidden.
func boundPreview(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			return r
		}
		return -1
	}, s)
	if text, cut := protocol.TruncateText(s, protocol.MaxApprovalPreview); cut {
		return text + " …[shortened]"
	}
	return s
}

// answerApproval turns an owner reaction on an approval message into the
// endpoint's interaction.respond for exactly that interaction. The reaction
// is one decision: it is claimed before the endpoint sees it, and a stale
// or repeated reaction never answers again.
func (c *Carrier) answerApproval(evt nostr.Event, messageID string, appr Approval, gesture string) error {
	if appr.Disabled || appr.Pending != nil {
		return nil // the message shows no remote answer now, or is changing
	}
	option := appr.RejectOption
	if gesture == approveReaction {
		option = appr.ApproveOption
	}
	if option == "" {
		return nil // this approval offers no one-tap answer for the gesture
	}
	// The approval belongs to a request this binding submitted: after a
	// change of owner, channel or native session, old approvals are not
	// answered.
	if rc, owned, err := c.ledger.ReceiptFor(appr.RequestRef); err != nil || !owned || !c.ownsReceipt(rc) {
		return err
	}
	cmd, _ := json.Marshal(map[string]string{"ref": appr.RequestRef, "interaction_id": appr.InteractionID, "option": option})
	if _, ok, err := c.claimReaction(evt, OpRespond, appr.Epoch, cmd); err != nil || !ok {
		return err
	}
	// An approve carries its proof: a harness that allows a call only with
	// the owner's signature (Claude, bead 611.42.4) verifies it itself.
	var evidence json.RawMessage
	if gesture == approveReaction {
		var err error
		if evidence, err = c.approveEvidence(evt, messageID, appr); err != nil {
			return err
		}
	}
	st := Settlement{Op: OpRespond, RequestRef: appr.RequestRef}
	out, err := c.handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: appr.RequestRef,
		TargetID: appr.Target, Epoch: appr.Epoch, InteractionID: appr.InteractionID, Option: option, Evidence: evidence,
	}, c.source(evt.ID.Hex(), ""))
	var refusal *protocol.Refusal
	reply, _ := out.(protocol.Reply)
	switch {
	case err == nil && reply.Outcome.Code == protocol.CodeAlreadyResolved && answeredWith(reply.Snapshot, appr.InteractionID, option):
		// A replay after a crash: this reaction's answer was delivered.
	case err == nil && reply.Outcome.Code == protocol.CodeAlreadyResolved && resolvedAs(reply.Snapshot, appr.InteractionID) == protocol.ResolutionDeliveryUnknown:
		// A replay whose answer may or may not have arrived: say exactly
		// that, never that it was already answered.
		return c.settleApprovalAnswer(evt, messageID, st, deliveryUnknownText)
	case err == nil && reply.Outcome.Code == protocol.CodeAlreadyResolved:
		return c.settleApprovalAnswer(evt, messageID, st, "Not sent: this approval was already answered.")
	case errors.As(err, &refusal) && refusal.Code == protocol.CodeAlreadyResolved:
		// The endpoint says why: resolved, or an earlier answer in flight.
		return c.settleApprovalAnswer(evt, messageID, st, "Not sent: "+protocol.InertInline(refusal.Message)+".")
	case gesture == approveReaction && errors.As(err, &refusal) && (refusal.Code == protocol.CodeInvalid || refusal.Code == protocol.CodeNativeError):
		// The harness could not verify the approve: it says why, in the
		// owner's words. Nothing was answered, so ✅ again or ❌ still works.
		return c.settleApprovalAnswer(evt, messageID, st, protocol.InertInline(refusal.Message))
	case err != nil:
		return c.settleApprovalAnswer(evt, messageID, st, "Not sent: "+protocol.InertInline(err.Error()))
	}
	// The outcome shows as an edit of the approval message when the record
	// resolves the interaction.
	_, err = c.ledger.Settle(evt.ID.Hex(), st)
	return err
}

// resolvedAs is how the record says the interaction ended, or "".
func resolvedAs(s protocol.Snapshot, interactionID string) protocol.ResolutionOutcome {
	for _, r := range s.Resolved {
		if r.InteractionID == interactionID {
			return r.Outcome
		}
	}
	return ""
}

// deliveryUnknownText is the owner-facing line for delivery_unknown.
const deliveryUnknownText = "The answer may not have reached the terminal; check the session."

// answeredWith reports whether the record says AMQ delivered option for the
// interaction.
func answeredWith(s protocol.Snapshot, interactionID, option string) bool {
	for _, r := range s.Resolved {
		if r.InteractionID == interactionID && r.Outcome == protocol.ResolutionAnswered && r.Option == option {
			return true
		}
	}
	return false
}

// settleApprovalAnswer replies under the approval message, then settles the
// reaction.
func (c *Carrier) settleApprovalAnswer(evt nostr.Event, messageID string, st Settlement, text string) error {
	if err := c.reply(evt, c.rowTags(messageID, ""), text); err != nil {
		return err
	}
	_, err := c.ledger.Settle(evt.ID.Hex(), st)
	return err
}

// editPrepared runs between preparing an edit and saving its receipt; tests
// crash the edge there.
var editPrepared = func() {}

// rootKey is a request's one root-row obligation; it sorts before every
// edit key of the same request, so Flush sends the root first.
func rootKey(ref string) string { return fmt.Sprintf("row/%s/%08d", ref, 0) }

// preparedRow is a stored row event and the revision it shows.
type preparedRow struct {
	nostr.Event
	revision int
}

// prepareRow signs and stores a row event for key, or returns the one
// already stored there.
func (c *Carrier) prepareRow(key string, evt nostr.Event, revision int) (preparedRow, error) {
	if err := c.sign(&evt); err != nil {
		return preparedRow{}, err
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		return preparedRow{}, err
	}
	stored, err := c.ledger.PrepareRevision(key, raw, revision, c.share())
	if err != nil {
		return preparedRow{}, err
	}
	var prepared nostr.Event
	if err := json.Unmarshal(stored.Event, &prepared); err != nil {
		return preparedRow{}, err
	}
	return preparedRow{Event: prepared, revision: stored.Revision}, nil
}

func (c *Carrier) prepare(key string, evt nostr.Event) error {
	if err := c.sign(&evt); err != nil {
		return err
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = c.ledger.Prepare(key, raw, c.share())
	return err
}

// Flush bounds for one pass (codex #866 r1 #8): at most flushBatch sends,
// each waiting at most publishTimeout for its OK, so a stalled relay never
// holds the edge.
const (
	flushBatch     = 16
	publishTimeout = 10 * time.Second
)

// Flush sends owed outputs in order and records acceptance on the relay's
// matching OK. Before each send it re-checks that the output belongs to this
// share (body and DM channel), that the enrolled generation still grants
// its kind, and that gate (the verified membership) still admits export;
// an output that fails stays owed and is never re-signed (codex #866 r1 #3,
// #6). An explicit rejection or an unknown result leaves it owed; the same
// stored bytes are sent again next time. The ledger lock is not held while
// a send waits.
func (c *Carrier) Flush(ctx context.Context, pub Publisher, gate func() error) error {
	c.mu.Lock()
	pending, err := c.ledger.Pending()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	sent := 0
	for _, o := range pending {
		if o.Binding != c.share() {
			continue // another binding's output: owed, never redirected
		}
		if sent == flushBatch {
			return nil // only eligible sends count (codex #866 r2 #8)
		}
		var evt nostr.Event
		if err := json.Unmarshal(o.Event, &evt); err != nil {
			return fmt.Errorf("outbox %s: %w", o.Key, err)
		}
		if _, err := c.grant(uint16(evt.Kind), c.now()); err != nil {
			return fmt.Errorf("%w: kind %d: %v", ErrNoGrant, evt.Kind, err)
		}
		if err := c.fence(); err != nil {
			return err // exported only while the approved native session is attached
		}
		if gate != nil {
			if err := gate(); err != nil {
				return err
			}
		}
		pctx, cancel := context.WithTimeout(ctx, publishTimeout)
		err := pub(pctx, evt)
		cancel()
		if err != nil {
			return fmt.Errorf("publish %s: %w", o.Key, err)
		}
		sent++
		c.mu.Lock()
		err = c.ledger.MarkAccepted(o.Key)
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// rowTags and editTags are the only place row tags are built (relay design
// B2/B3, slice 4 contract §3): a row lives in the owner DM channel (h),
// addresses the owner (p) and replies to the owner's input event (e ...
// reply) when it has one, naming the input's thread root (e ... root) when
// the input was itself a reply; an edit names its original row (e) in the
// same channel.
func (c *Carrier) rowTags(inputID, root string) nostr.Tags {
	tags := nostr.Tags{{"h", c.binding.Channel}, {"p", c.binding.Owner}}
	if validHexID(root) && root != inputID {
		tags = append(tags, nostr.Tag{"e", root, "", "root"})
	}
	if validHexID(inputID) {
		tags = append(tags, nostr.Tag{"e", inputID, "", "reply"})
	}
	return tags
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

// snapshotText renders one revision of a request for its row: target, state,
// code and reason, then the result, shortened with an explicit marker. The
// request ref is an internal id and never shown to the owner (611.52).
func snapshotText(s protocol.Snapshot, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", s.TargetID, s.State)
	if s.Code != "" {
		fmt.Fprintf(&b, " (%s)", s.Code)
	}
	if reason != "" {
		// The reason is adapter text (a stored refusal); render it inert so
		// the DM renderer cannot decode entities or autolink URLs inside it
		// (review of #972 r2).
		fmt.Fprintf(&b, ": %s", protocol.InertInline(reason))
	}
	if s.Result != nil && s.Result.Text != "" {
		text := s.Result.Text
		if len(text) > maxRowText || s.Result.Truncated {
			for len(text) > maxRowText || !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			text += "\n[shortened]"
		}
		b.WriteString("\n\n" + text)
	}
	return b.String()
}
