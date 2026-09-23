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
	if err := c.eligible(now); err != nil {
		return err
	}
	if _, settled, err := c.ledger.Settled(evt.ID.Hex()); err != nil || settled {
		return err
	}
	if err := c.fence(); err != nil {
		return nil // a replacement native session is never acted on for a reaction
	}
	cmd, _ := json.Marshal(map[string]string{"ref": ref})
	claim, _, err := c.ledger.Claim(c.claimFor(evt, OpCancel, "", "", created.Add(MutationWindow), cmd))
	if err != nil || !c.owns(claim) {
		return err
	}
	return c.statusOrCancel(evt, OpCancel, ref, created.Add(MutationWindow))
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
		return c.answer(evt, err.Error())
	}
	// Every command, not only submit, runs only against the approved native
	// session: /inspect of a replacement session is never answered (codex
	// #866 r2 #7).
	if err := c.fence(); err != nil {
		return c.settleAnswer(evt, Settlement{Op: n.Op, State: "refused"}, err.Error())
	}
	claim := c.claimFor(evt, n.Op, n.RequestID, "", n.NotAfter, nil)
	if n.Op == OpSubmit {
		// The epoch is fixed at first sight and stored in the claim, so a
		// later import never retargets a new epoch.
		s, err := c.inspect()
		if err != nil {
			return c.answer(evt, "cannot reach the shared session: "+err.Error())
		}
		claim.Epoch = s.Epoch
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
			return c.settleAnswer(evt, Settlement{Op: claim.Op}, "inspect failed: "+err.Error())
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
				MinEvidence: string(protocol.EvidenceAdmitted),
			},
		}
		out, err := c.handle(cmd, src)
		if err != nil {
			return c.settleAnswer(evt, Settlement{Op: claim.Op, RequestRef: ref, State: "refused"}, "submit refused: "+err.Error())
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
		return c.settleAnswer(evt, Settlement{Op: op, RequestRef: ref, State: "refused"}, op+" failed: "+err.Error())
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
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(c.now().Unix()),
		Kind:      KindDM,
		Tags:      c.replyTags(to),
		Content:   text,
	}
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
