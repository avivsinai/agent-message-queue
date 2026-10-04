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

// ApproveEvidence is the proof an owner ✅ carries to a harness that allows
// a call only with the owner's signature (bead 611.42.4): the owner's signed
// reaction, the signed approval message it targets, and every edit of that
// message the relay holds, all as received and stored, never re-encoded
// from parts. EditsComplete is false when the edits could not be read; such
// evidence never proves an allow.
type ApproveEvidence struct {
	Reaction      nostr.Event   `json:"reaction"`
	Message       nostr.Event   `json:"message"`
	Edits         []nostr.Event `json:"edits"`
	EditsComplete bool          `json:"edits_complete"`
}

// EditFetcher returns every kind 40003 edit of messageID that the relay
// holds from the body key. An error means the set is not known.
type EditFetcher func(ctx context.Context, messageID string) ([]nostr.Event, error)

// editFetchTimeout bounds the edit read an owner ✅ waits for.
const editFetchTimeout = 10 * time.Second

// RelayEdits reads the edits of an approval message from the relay: kind
// 40003 events by body that name the message in an e tag, up to the
// relay's end of stored events. A subscription that ends, overflows or
// times out first is an error, never a partial set.
func RelayEdits(conn *relay.Conn, body string) EditFetcher {
	return func(ctx context.Context, messageID string) ([]nostr.Event, error) {
		author, err := nostr.PubKeyFromHex(body)
		if err != nil {
			return nil, fmt.Errorf("body pubkey: %w", err)
		}
		sub, err := conn.Subscribe(ctx, fmt.Sprintf("edits-%d", time.Now().UnixNano()),
			nostr.Filter{Kinds: []nostr.Kind{KindEdit}, Authors: []nostr.PubKey{author}, Tags: nostr.TagMap{"e": {messageID}}})
		if err != nil {
			return nil, err
		}
		defer sub.Close()
		var edits []nostr.Event
		for {
			select {
			case evt := <-sub.Events:
				edits = append(edits, evt)
			case <-sub.EOSE:
				// Stored events are queued before EOSE; drain them first.
				for {
					select {
					case evt := <-sub.Events:
						edits = append(edits, evt)
					default:
						return edits, nil
					}
				}
			case <-sub.Done():
				return nil, fmt.Errorf("edit read ended: %v", sub.Err())
			case <-ctx.Done():
				return nil, errors.New("edit read timed out")
			}
		}
	}
}

// SetEditFetcher sets how an owner ✅ reads the approval message's edits;
// nil marks every approve's evidence incomplete.
func (c *Carrier) SetEditFetcher(f EditFetcher) {
	c.editsMu.Lock()
	defer c.editsMu.Unlock()
	c.edits = f
}

// approveEvidence is the evidence for an owner ✅ on the approval message
// posted for appr: the original kind 9 message from the outbox, whose id
// the reaction names, and the edits of it the relay holds.
func (c *Carrier) approveEvidence(evt nostr.Event, messageID string, appr Approval) (json.RawMessage, error) {
	posted, ok, err := c.ledger.Prepared(approvalKey(appr.RequestRef, appr.InteractionID))
	if err != nil || !ok {
		return nil, fmt.Errorf("approval message for %s not stored: %v", appr.InteractionID, err)
	}
	var msg nostr.Event
	if err := json.Unmarshal(posted.Event, &msg); err != nil {
		return nil, err
	}
	if msg.ID.Hex() != messageID {
		return nil, fmt.Errorf("stored approval message %s is not the reacted message %s", msg.ID.Hex(), messageID)
	}
	ev := ApproveEvidence{Reaction: evt, Message: msg}
	c.editsMu.Lock()
	fetch := c.edits
	c.editsMu.Unlock()
	if fetch != nil {
		ctx, cancel := context.WithTimeout(context.Background(), editFetchTimeout)
		edits, err := fetch(ctx, messageID)
		cancel()
		if err == nil {
			ev.Edits, ev.EditsComplete = edits, true
		}
	}
	return json.Marshal(ev)
}

// VerifyApproveEvidence checks an owner ✅ as proof that the owner allowed
// the call that prompt shows. Every check must pass:
//
//   - the reaction is kind 7, its id is the hash of its content and its
//     BIP-340 signature verifies under owner (64 lowercase hex);
//   - its content is the approve gesture ✅ and its last e tag is the
//     message's id;
//   - it is dated within [notBefore, notAfter];
//   - the message is kind 9, its id and signature verify, and its content
//     is exactly the approval text this carrier posts for prompt, with
//     either trailer (an approval offered reject only at first is edited to
//     offer ✅ when it binds; the edit changes the trailer, not the prompt);
//   - the edits are complete, and each is a kind 40003 by the message's
//     author naming the message, whose id and signature verify, and whose
//     content shows the same prompt under the same ref with one of this
//     carrier's own trailers. An edit can change what the owner saw, so an
//     edit that shows anything else refuses.
//
// The caller renders prompt itself, so the content checks prove the owner
// saw exactly that prompt. A relay that hides an edit from the query
// defeats the edit check; the trust anchor is the owner's reaction plus
// the relay's honest edit history.
func VerifyApproveEvidence(raw json.RawMessage, owner, prompt string, notBefore, notAfter time.Time) error {
	var ev ApproveEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	r, m := ev.Reaction, ev.Message
	switch {
	case !validHexID(owner):
		return errors.New("no valid pinned owner")
	case r.Kind != KindReaction:
		return errors.New("reaction is not kind 7")
	case !r.CheckID() || !r.VerifySignature():
		return errors.New("reaction id or signature does not verify")
	case r.PubKey.Hex() != owner:
		return errors.New("reaction is not signed by the pinned owner")
	case strings.TrimSpace(r.Content) != approveReaction:
		return errors.New("reaction is not the approve gesture")
	case lastETag(r) != m.ID.Hex():
		return errors.New("reaction does not target the approval message")
	}
	created := time.Unix(int64(r.CreatedAt), 0)
	if created.Before(notBefore) || created.After(notAfter) {
		return errors.New("reaction is outside the approval's window")
	}
	switch {
	case m.Kind != KindDM:
		return errors.New("approval message is not kind 9")
	case !m.CheckID() || !m.VerifySignature():
		return errors.New("approval message id or signature does not verify")
	}
	ref, ok := approvalRef(m.Content, prompt)
	if !ok || !slices.Contains(approvalVariants(ref, prompt)[:2], m.Content) {
		return errors.New("approval message does not show this call")
	}
	if !ev.EditsComplete {
		return errors.New("the approval message's edits could not be read")
	}
	allowed := approvalVariants(ref, prompt)
	for _, ed := range ev.Edits {
		switch {
		case ed.Kind != KindEdit || ed.PubKey != m.PubKey || lastETag(ed) != m.ID.Hex():
			return errors.New("an edit is not an edit of the approval message")
		case !ed.CheckID() || !ed.VerifySignature():
			return errors.New("an edit's id or signature does not verify")
		case !slices.Contains(allowed, ed.Content):
			return errors.New("an edit changed what the approval message shows")
		}
	}
	return nil
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
	if !ok || boundPreview(prompt) != prompt {
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
