package buzzio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Deletion kinds Buzz clients apply to a message: a NIP-09 deletion request
// and the Buzz-native NIP-29 delete-event, after which the relay hides the
// target (Buzz desktop formatTimelineMessages and mobile formatTimeline
// treat both alike, by their e tags). The carrier never publishes either.
const (
	KindDeletion      = 5
	KindGroupDeletion = 9005
)

// ApproveEvidence is the proof an owner ✅ carries to a harness that allows
// a call only with the owner's signature (bead 611.42.4): the owner's signed
// reaction and the signed approval message it targets, both as received
// and stored, never re-encoded from parts. The message's edit and deletion
// history is never taken from the evidence: the verifier reads it from the
// relay itself (CheckHistory).
type ApproveEvidence struct {
	Reaction nostr.Event `json:"reaction"`
	Message  nostr.Event `json:"message"`
}

// ApproveCheck is what an allow must prove, all from the verifier's own
// trusted state, never from the evidence: the pinned owner, the share's
// body pubkey, DM channel and target, the verifier's own rendering of the
// call, and the window the reaction must be dated in.
type ApproveCheck struct {
	Owner, Body, Channel, Target string
	Prompt                       string
	NotBefore, NotAfter          time.Time
}

// ErrAltered is an approval message whose shown call changed after it was
// posted: an edit that shows another call, any deletion by the body, or
// any deletion by the owner since the message. The owner may have approved text other than the call.
var ErrAltered = errors.New("the approval message was altered after it was posted")

// approveEvidence is the evidence for an owner ✅ on the approval message
// posted for appr: the original kind 9 message from the outbox, whose id
// the reaction names.
func (c *Carrier) approveEvidence(evt nostr.Event, messageID string, appr Approval) (json.RawMessage, error) {
	msg, err := c.storedApproval(messageID, appr)
	if err != nil {
		return nil, err
	}
	return json.Marshal(ApproveEvidence{Reaction: evt, Message: msg})
}

// storedApproval is the kind 9 message posted for appr, read from the
// outbox; its id must be messageID.
func (c *Carrier) storedApproval(messageID string, appr Approval) (nostr.Event, error) {
	posted, ok, err := c.ledger.Prepared(approvalKey(appr.RequestRef, appr.InteractionID))
	if err != nil || !ok {
		return nostr.Event{}, fmt.Errorf("approval message for %s not stored: %v", appr.InteractionID, err)
	}
	var msg nostr.Event
	if err := json.Unmarshal(posted.Event, &msg); err != nil {
		return nostr.Event{}, err
	}
	if msg.ID.Hex() != messageID {
		return nostr.Event{}, fmt.Errorf("stored approval message %s is not the reacted message %s", msg.ID.Hex(), messageID)
	}
	return msg, nil
}

// VerifyApproveEvidence checks an owner ✅ as proof that the owner allowed
// the call want.Prompt shows, in the share want names, and returns the
// approval message. Every check must pass:
//
//   - the reaction is kind 7, its id is the hash of its content and its
//     BIP-340 signature verifies under want.Owner;
//   - its content is the approve gesture ✅ and its last e tag is the
//     message's id;
//   - it is dated within [want.NotBefore, want.NotAfter];
//   - the message is kind 9, its id and signature verify, it is signed by
//     the share's body key and posted in the share's DM channel (h tag);
//   - its content is exactly the approval text this carrier posts for
//     want.Prompt, under a request ref of the share's target, with either
//     trailer (an approval offered reject only at first is edited to offer
//     ✅ when it binds; the edit changes the trailer, not the prompt).
//
// The caller renders the prompt itself, so the content checks prove the
// owner saw exactly that prompt in the original message. Edits and
// deletions are CheckHistory's.
func VerifyApproveEvidence(raw json.RawMessage, want ApproveCheck) (nostr.Event, error) {
	var ev ApproveEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nostr.Event{}, fmt.Errorf("evidence: %w", err)
	}
	r, m := ev.Reaction, ev.Message
	switch {
	case !validHexID(want.Owner) || !validHexID(want.Body) || want.Channel == "" || want.Target == "":
		return m, errors.New("no complete pinned owner and share")
	case r.Kind != KindReaction:
		return m, errors.New("reaction is not kind 7")
	case !r.CheckID() || !r.VerifySignature():
		return m, errors.New("reaction id or signature does not verify")
	case r.PubKey.Hex() != want.Owner:
		return m, errors.New("reaction is not signed by the pinned owner")
	case strings.TrimSpace(r.Content) != approveReaction:
		return m, errors.New("reaction is not the approve gesture")
	case lastETag(r) != m.ID.Hex():
		return m, errors.New("reaction does not target the approval message")
	}
	created := time.Unix(int64(r.CreatedAt), 0)
	if created.Before(want.NotBefore) || created.After(want.NotAfter) {
		return m, errors.New("reaction is outside the approval's window")
	}
	switch {
	case m.Kind != KindDM:
		return m, errors.New("approval message is not kind 9")
	case !m.CheckID() || !m.VerifySignature():
		return m, errors.New("approval message id or signature does not verify")
	case m.PubKey.Hex() != want.Body:
		return m, errors.New("approval message is not from this share's body")
	case tagValue(m, "h") != want.Channel:
		return m, errors.New("approval message is not in this share's DM channel")
	}
	ref, ok := approvalRef(m.Content, want.Prompt)
	if !ok || !slices.Contains(approvalVariants(ref, want.Prompt)[:2], m.Content) {
		return m, errors.New("approval message does not show this call")
	}
	if _, target, _, err := protocol.DecodeRef(ref); err != nil || target != want.Target {
		return m, errors.New("approval message is for another target")
	}
	return m, nil
}

// historyTimeout bounds CheckHistory's relay reads.
const historyTimeout = 10 * time.Second

// maxHistoryEdits bounds the edits read of one approval message. The
// carrier edits it a few times at most; a full page means the history is
// not known, because the relay may have cut older edits from it. The bound
// stays below common relay page caps, so a capped page is a full one.
const maxHistoryEdits = 20

// CheckHistory reads the approval message's history from the relay, each
// read up to the relay's end of stored events, within historyTimeout. It
// covers every way a Buzz client changes or hides a message: edits (kind
// 40003, by the body or by the owner, whom Buzz lets edit the agent's
// messages) and deletions (kinds 5 and 9005, by e tag):
//
//   - every edit naming the message must show the same call under the same
//     ref, with only one of this carrier's own trailers or outcomes, and
//     carry no imeta attachment;
//   - no deletion may name the message or any of its edits, from anyone;
//   - no deletion by the body may exist at all, and no deletion by the
//     owner since the message, wherever it points: the relay hides a
//     deleted edit, so a deletion of an edit no read returns cannot be told
//     apart from an unrelated one.
//
// An edit or deletion that fails is ErrAltered. A read that errors, ends
// early, overflows or times out is another error: the history is not
// known. The relay is trusted to return the full history.
func CheckHistory(ctx context.Context, conn *relay.Conn, msg nostr.Event, want ApproveCheck) error {
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	ref, ok := approvalRef(msg.Content, want.Prompt)
	if !ok {
		return errors.New("approval message does not show this call")
	}
	owner, err := nostr.PubKeyFromHex(want.Owner)
	if err != nil {
		return fmt.Errorf("owner pubkey: %w", err)
	}
	authors := []nostr.PubKey{msg.PubKey, owner}
	deletionKinds := []nostr.Kind{KindDeletion, KindGroupDeletion}
	edits, err := readStored(ctx, conn, nostr.Filter{Kinds: []nostr.Kind{KindEdit}, Authors: authors, Tags: nostr.TagMap{"e": {msg.ID.Hex()}}, Limit: maxHistoryEdits})
	if err != nil {
		return fmt.Errorf("read the approval message's edits: %w", err)
	}
	// The relay returns its newest page; a full one may hide an older edit
	// (review of #936 r4).
	if len(edits) >= maxHistoryEdits {
		return fmt.Errorf("the approval message has %d or more edits; its history is not known", maxHistoryEdits)
	}
	allowed := approvalVariants(ref, want.Prompt)
	ids := []string{msg.ID.Hex()}
	for _, ed := range edits {
		if ed.Kind != KindEdit || !slices.Contains(allowed, ed.Content) || hasTagName(ed, "imeta") {
			return fmt.Errorf("%w: an edit shows another call", ErrAltered)
		}
		ids = append(ids, ed.ID.Hex())
	}
	targeted, err := readStored(ctx, conn, nostr.Filter{Kinds: deletionKinds, Tags: nostr.TagMap{"e": ids}})
	if err != nil {
		return fmt.Errorf("read the deletions of the message and its edits: %w", err)
	}
	if len(targeted) > 0 {
		return fmt.Errorf("%w: the message or an edit of it was deleted", ErrAltered)
	}
	// A deletion's created_at is its signer's claim, so the body's
	// deletions are read with no time bound: AMQ never deletes, and a
	// backdated one would otherwise hide a deleted edit (review of #936 r3).
	byBody, err := readStored(ctx, conn, nostr.Filter{Kinds: deletionKinds, Authors: []nostr.PubKey{msg.PubKey}})
	if err != nil {
		return fmt.Errorf("read the body's deletions: %w", err)
	}
	if len(byBody) > 0 {
		return fmt.Errorf("%w: %d deletion(s) by the body", ErrAltered, len(byBody))
	}
	byOwner, err := readStored(ctx, conn, nostr.Filter{Kinds: deletionKinds, Authors: []nostr.PubKey{owner}, Since: msg.CreatedAt})
	if err != nil {
		return fmt.Errorf("read the owner's deletions since the message: %w", err)
	}
	if len(byOwner) > 0 {
		return fmt.Errorf("%w: %d deletion(s) by the owner since the message", ErrAltered, len(byOwner))
	}
	return nil
}

// hasTagName reports a tag named name.
func hasTagName(evt nostr.Event, name string) bool {
	for _, t := range evt.Tags {
		if len(t) > 0 && t[0] == name {
			return true
		}
	}
	return false
}

// readStored reads every stored event matching filter, up to EOSE. The
// relay client delivers only events whose id and signature verify and that
// match the filter.
func readStored(ctx context.Context, conn *relay.Conn, filter nostr.Filter) ([]nostr.Event, error) {
	sub, err := conn.Subscribe(ctx, fmt.Sprintf("approve-history-%d", time.Now().UnixNano()), filter)
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

// approvalRef is the request ref of an approval text for prompt shown
// whole, or false.
func approvalRef(content, prompt string) (string, bool) {
	rest, ok := strings.CutPrefix(content, "Approval needed (")
	if !ok || protocol.BoundPreview(prompt) != prompt {
		return "", false
	}
	ref, _, ok := strings.Cut(rest, "):\n\n")
	if !ok {
		return "", false
	}
	if _, _, _, err := protocol.DecodeRef(ref); err != nil {
		return "", false
	}
	return ref, true
}

// approvalVariants are every text this carrier posts or edits for one
// approval of prompt under ref: first the two a pending approval is posted
// with (✅ and ❌, ❌ alone), then the other instruction trailers and every
// outcome. Only the trailer differs between them.
func approvalVariants(ref, prompt string) []string {
	base := Approval{RequestRef: ref, Prompt: prompt}
	both, reject, approve, disabled := base, base, base, base
	both.ApproveOption, both.RejectOption = "✅", "❌"
	reject.RejectOption = "❌"
	approve.ApproveOption = "✅"
	disabled.Disabled = true
	out := []string{approvalText(both, ""), approvalText(reject, ""), approvalText(approve, ""), approvalText(disabled, ""), approvalText(base, "")}
	for _, r := range []protocol.Resolution{
		{Outcome: protocol.ResolutionAnswered, Option: "✅"}, {Outcome: protocol.ResolutionAnswered, Option: "❌"},
		{Outcome: protocol.ResolutionElsewhere}, {Outcome: protocol.ResolutionRunEnded}, {Outcome: protocol.ResolutionDeliveryUnknown},
	} {
		out = append(out, approvalText(both, outcomeText(r, both)))
	}
	return out
}
