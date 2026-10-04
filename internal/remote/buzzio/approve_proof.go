package buzzio

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// ApproveEvidence is the proof an owner ✅ carries to a harness that allows
// a call only with the owner's signature (bead 611.42.4): the owner's signed
// reaction and the signed approval message it targets, both as received
// and stored, never re-encoded from parts.
type ApproveEvidence struct {
	Reaction nostr.Event `json:"reaction"`
	Message  nostr.Event `json:"message"`
}

// approveEvidence is the evidence for an owner ✅ on the approval message
// posted for appr: the original kind 9 message from the outbox, whose id
// the reaction names. The edits that change its trailer are not part of it.
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
	return json.Marshal(ApproveEvidence{Reaction: evt, Message: msg})
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
//     offer ✅ when it binds; the edit changes the trailer, not the prompt).
//
// The caller renders prompt itself, so the content check proves the owner
// saw exactly that prompt.
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
	case !approvalTextShows(m.Content, prompt):
		return errors.New("approval message does not show this call")
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

// approvalTextShows reports whether content is approvalText for prompt,
// shown whole, under a valid request ref, offering ✅ and ❌ or ❌ alone.
func approvalTextShows(content, prompt string) bool {
	rest, ok := strings.CutPrefix(content, "Approval needed (")
	if !ok || boundPreview(prompt) != prompt {
		return false
	}
	ref, _, ok := strings.Cut(rest, "):\n\n")
	if !ok {
		return false
	}
	if _, _, _, err := protocol.DecodeRef(ref); err != nil {
		return false
	}
	for _, approve := range []string{"✅", ""} {
		if content == approvalText(Approval{RequestRef: ref, Prompt: prompt, ApproveOption: approve, RejectOption: "❌"}, "") {
			return true
		}
	}
	return false
}
