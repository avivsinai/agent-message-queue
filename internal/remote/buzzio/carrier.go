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

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Kinds the edge publishes (enrolled via share --enable buzz-dm).
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
// grants, so AMQ refuses to sign locally.
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
	// prunedAt is when Flush last pruned settled outputs.
	prunedAt time.Time

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
	return relay.Sign(evt, s.secret)
}

// source is this carrier's identity to the endpoint: a stable,
// protocol-valid host derived from relay, body and owner. It carries no
// authority beyond naming the creator of requests submitted here, and the
// approved native session they may run in: the one the fence proves
// attached before every submit, which core checks again at the handoff.
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
		NativeSession: c.binding.NativeSession,
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

// threadRoot is the NIP-10 root an owner event replies within, if any. A
// root-marked e tag wins. Buzz marks a direct reply to a thread's first
// message with a single reply-marked e tag, so that parent is the root;
// a reply-marked tag beside other e tags names an intermediate parent and
// proves no root. With no markers, the legacy positional form names the
// root first. A reply to a threaded event that names no root is refused by
// the relay ("root tag does not match thread ancestry").
func threadRoot(evt nostr.Event) string {
	var es []nostr.Tag
	marked := false
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "e" {
			es = append(es, t)
			marked = marked || len(t) >= 4 && t[3] != ""
		}
	}
	for _, t := range es {
		if len(t) >= 4 && t[3] == "root" && validHexID(t[1]) {
			return t[1]
		}
	}
	switch {
	case len(es) == 0:
		return ""
	case !marked:
		if validHexID(es[0][1]) {
			return es[0][1]
		}
	case len(es) == 1 && es[0][3] == "reply" && validHexID(es[0][1]):
		return es[0][1]
	}
	return ""
}

// KindReaction is a NIP-25 reaction.
const KindReaction = 7

// cancelReaction is the owner's cancel gesture on a result row.
const cancelReaction = "❌"

// IngestReaction handles one verified kind 7 event from the owner. On one
// of this edge's result rows, ❌ cancels exactly that row's request; the row
// is resolved through the persisted receipt mapping, not the reaction's own
// tags, and a reaction carries no h tag it has to match. Anything else is
// ignored; removing a reaction never undoes a cancel.
func (c *Carrier) IngestReaction(evt nostr.Event) error {
	if evt.Kind != KindReaction || evt.PubKey.Hex() != c.binding.Owner {
		return nil
	}
	target := lastETag(evt) // NIP-25: the last e tag is the reacted-to event
	if target == "" || strings.TrimSpace(evt.Content) != cancelReaction {
		return nil
	}
	ref, ok, err := c.ledger.RequestForRow(target)
	if err != nil || !ok {
		return err
	}
	cmd, _ := json.Marshal(map[string]string{"ref": ref})
	notAfter, _, ok, err := c.claimReaction(evt, OpCancel, "", cmd)
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
func (c *Carrier) claimReaction(evt nostr.Event, op, epoch string, cmd json.RawMessage) (time.Time, Claim, bool, error) {
	now := c.now()
	created := time.Unix(int64(evt.CreatedAt), 0)
	notAfter := created.Add(MutationWindow)
	if created.After(now.Add(maxFutureSkew)) || now.After(notAfter) {
		return notAfter, Claim{}, false, nil // a stale gesture never executes
	}
	if err := c.eligible(now); err != nil {
		return notAfter, Claim{}, false, err
	}
	if _, settled, err := c.ledger.Settled(evt.ID.Hex()); err != nil || settled {
		return notAfter, Claim{}, false, err
	}
	if err := c.fence(); err != nil {
		return notAfter, Claim{}, false, nil // a replacement native session is never acted on for a reaction
	}
	claim, _, err := c.ledger.Claim(c.claimFor(evt, op, "", epoch, notAfter, cmd))
	if err != nil || !c.owns(claim) {
		return notAfter, Claim{}, false, err
	}
	return notAfter, claim, true, nil
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
// going to the DM channel.
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
	// answer.
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
	// session: /inspect of a replacement session is never answered.
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
// target) is never replayed here.
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
// wire schema).
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
// one. A claim that already existed but has no settlement is an interrupted
// import: the core's own decision is read back
// first and only a request the core never saw is submitted, so an
// interrupted busy rejection never turns into execution. The settlement is
// recorded only after the row is prepared.
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
		// resubmitted.
		_, answered, err := c.ledger.Prepared("direct/" + evt.ID.Hex())
		if err != nil {
			return err
		}
		// The root row is committed before its id reaches the receipt, so the
		// prepared root obligation itself is checked, and the receipt is
		// restored from it.
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
		var refusal *protocol.Refusal
		if err != nil && !errors.As(err, &refusal) {
			// An untyped error leaves the core's decision unknown: it may
			// hold the request. Read it back; only a request the core does
			// not have is shown refused.
			got, gerr := c.handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, src)
			switch {
			case gerr == nil:
				out, err = got, nil
			case protocol.RefusalCode(gerr) != protocol.CodeNotFound:
				return gerr // unknown: stay unsettled and retry on the next delivery
			}
		}
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
// epoch and the owner command's signed deadline.
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
// a failed answer leaves the command unsettled, so its redelivery answers it.
// The answer is keyed by the event, so a retry returns the stored one.
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
// dated strictly after the previous one. Returning nil means the output is
// durably owed in the outbox; Flush delivers it.
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
	if int(snap.Revision) <= rc.Revision {
		return nil // this or a newer revision is already prepared
	}
	now := c.now().Unix()
	if now <= rc.LastEditAt {
		return ErrClockBehind
	}
	// Reserve the edit's second before preparing it: after a crash between
	// the two, the next edit is still dated strictly later than any edit
	// already prepared.
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

// editPrepared runs between preparing an edit and saving its receipt; tests
// crash the edge there.
var editPrepared = func() {}

// rootKey is a request's one root-row obligation. It is prepared before
// any edit of the same request, so Flush sends it
// first.
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

// Flush bounds for one pass: at most flushBatch sends, each waiting at most
// publishTimeout for its OK, so a stalled relay never
// holds the edge.
const (
	flushBatch     = 16
	publishTimeout = 10 * time.Second
)

// Flush prunes settled outputs at most once per pruneEvery, at most
// pruneBatch per pass, so the outbox it scans every second stays bounded by
// what is still owed or recent.
const (
	pruneEvery = time.Minute
	pruneBatch = 256
)

// Flush sends owed outputs in order and records acceptance on the relay's
// matching OK. Before each send it re-checks that the output belongs to this
// share (body and DM channel), that the enrolled generation still grants
// its kind, and that gate (the verified membership) still admits export;
// an output that fails stays owed and is never re-signed. An unknown result
// or a lost connection stops the pass: the same stored bytes are sent again
// next time, in order. A negative OK describes that attempt only, so the
// output stays owed: its refusal and reason are recorded and it is retried
// after a backoff. Order is kept per request: a request's root row and edits go out in the order they were prepared
// (the outbox sequence, never key text), and while an earlier output of a
// request is owed (refused or not yet due), that request's later outputs
// wait; other requests' outputs still go out. A root whose positive OK was
// lost and whose retry the relay refuses as too old stays owed and holds
// its request's later outputs until has, when given, finds the exact signed
// event on the relay: verified presence proves the earlier send was stored,
// so the output is accepted and its request's later outputs go out; a lookup
// that fails or finds nothing leaves it owed. The ledger lock is not held
// while a send waits.
func (c *Carrier) Flush(ctx context.Context, pub Publisher, has EventLookup, gate func() error) error {
	c.mu.Lock()
	if now := c.now(); now.Sub(c.prunedAt) >= pruneEvery {
		// Pruning only bounds the outbox scan: a failure leaves the
		// records for the next pass and never holds a send.
		c.prunedAt = now
		_, _ = c.ledger.PruneSettled(pruneBatch)
	}
	pending, err := c.ledger.Pending()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	sent := 0
	waiting := map[string]bool{} // requests whose earlier output is still owed
	// Outputs written before sequence numbers (Seq 0) have no provable order
	// within their request: while any of them is refused and waiting out its
	// backoff, the request's other old outputs wait too.
	for _, o := range pending {
		if o.Seq == 0 && o.Binding == c.share() && !o.Due(c.now()) {
			waiting[outputGroup(o.Key)] = true
		}
	}
	for _, o := range pending {
		if o.Binding != c.share() {
			continue // another binding's output: owed, never redirected
		}
		group := outputGroup(o.Key)
		if waiting[group] {
			continue
		}
		if !o.Due(c.now()) {
			waiting[group] = true
			continue
		}
		if sent == flushBatch {
			return nil // only eligible sends count
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
		sent++
		var remote *relay.RemoteError
		switch {
		case err == nil:
			c.mu.Lock()
			err = c.ledger.MarkAccepted(o.Key, c.now())
			c.mu.Unlock()
		case errors.As(err, &remote) && errors.Is(remote.Kind, relay.ErrRejected) && c.storedOnRelay(ctx, has, evt.ID):
			c.mu.Lock()
			err = c.ledger.MarkAccepted(o.Key, c.now())
			c.mu.Unlock()
		case errors.As(err, &remote) && errors.Is(remote.Kind, relay.ErrRejected):
			waiting[group] = true
			c.mu.Lock()
			err = c.ledger.MarkRefused(o.Key, remote.Reason, c.now())
			c.mu.Unlock()
		default:
			return fmt.Errorf("publish %s: %w", o.Key, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// EventLookup reports whether the relay holds the event with this exact id.
type EventLookup func(ctx context.Context, id nostr.ID) (bool, error)

// StoredEventLookup looks an event up by id among conn's stored events. The
// relay client delivers only events whose id and signature verify, so a
// match is the exact signed event.
func StoredEventLookup(conn *relay.Conn) EventLookup {
	return func(ctx context.Context, id nostr.ID) (bool, error) {
		events, err := readStored(ctx, conn, nostr.Filter{IDs: []nostr.ID{id}, Limit: 1})
		if err != nil {
			return false, err
		}
		for _, evt := range events {
			if evt.ID == id {
				return true, nil
			}
		}
		return false, nil
	}
}

// storedOnRelay is whether has finds the refused event on the relay. A
// failed or absent lookup proves nothing.
func (c *Carrier) storedOnRelay(ctx context.Context, has EventLookup, id nostr.ID) bool {
	if has == nil {
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	found, err := has(lctx, id)
	return err == nil && found
}

// outputGroup is the ordering scope of an outbox key: one request's rows
// ("row/<ref>/..."), or the key itself.
func outputGroup(key string) string {
	if rest, ok := strings.CutPrefix(key, "row/"); ok {
		if ref, _, ok := strings.Cut(rest, "/"); ok {
			return "row/" + ref
		}
	}
	return key
}

// rowTags and editTags are the only place row tags are built: a row lives in
// the owner DM channel (h), addresses the owner (p) and replies to the
// owner's input event (e ... reply) when it has one, naming the input's
// thread root (e ... root) when the input was itself a reply; an edit names
// its original row (e) in the same channel.
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
// request ref is an internal id and never shown to the owner.
func snapshotText(s protocol.Snapshot, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", s.TargetID, s.State)
	if s.Code != "" {
		fmt.Fprintf(&b, " (%s)", s.Code)
	}
	if reason != "" {
		// The reason is adapter text (a stored refusal); render it inert so
		// the DM renderer cannot decode entities or autolink URLs inside it.
		fmt.Fprintf(&b, ": %s", protocol.InertInline(reason))
	}
	if s.State == protocol.StateRejected && s.Code == protocol.CodeBusy {
		// A settled DM never runs again, so a new message is the only retry.
		b.WriteString("\nNothing ran; send the message again when the session is free.")
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

// readStored reads every stored event matching filter, up to EOSE. The
// relay client delivers only events whose id and signature verify and that
// match the filter.
func readStored(ctx context.Context, conn *relay.Conn, filter nostr.Filter) ([]nostr.Event, error) {
	sub, err := conn.Subscribe(ctx, fmt.Sprintf("stored-event-%d", time.Now().UnixNano()), filter)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	var out []nostr.Event
	for {
		select {
		case evt := <-sub.Events:
			out = append(out, evt)
		case <-sub.EOSE:
			// Stored events are queued before EOSE; drain them first.
			for {
				select {
				case evt := <-sub.Events:
					out = append(out, evt)
				default:
					return out, nil
				}
			}
		case <-sub.Done():
			return nil, fmt.Errorf("the read ended early: %v", sub.Err())
		case <-ctx.Done():
			return nil, errors.New("the read timed out")
		}
	}
}

// lastETag is the event a NIP-25 reaction targets: its last e tag.
func lastETag(evt nostr.Event) string {
	target := ""
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "e" {
			target = t[1]
		}
	}
	return target
}
