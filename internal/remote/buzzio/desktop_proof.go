package buzzio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// A Claude tool call driven from Buzz Desktop has no relay share: the Desktop
// agent (amq-acp) holds the agent key, and the PermissionRequest hook holds
// no key at all. The agent posts the approval under its own key, reads the
// owner's signed ✅ and the message's edits and deletions from the relay, and
// then signs a grant over the message and the reaction. The hook checks both
// events and the grant against its pinned owner, agent key and DM channel,
// and compares the message with its own rendering of the call. The agent
// key never leaves amq-acp, and the grant names one message, which names one
// interaction and one action hash.

// DesktopApprovalText is the one text a Desktop approval that offers ✅ is
// posted with. prompt is the hook's signed prompt, which ends with the
// interaction id and the action hash. Both the poster and the verifier
// call it, so the owner's ✅ covers exactly the call the hook renders.
func DesktopApprovalText(prompt string) string {
	return "Approval needed:\n\n" + prompt + "\n\nReact ✅ to approve or ❌ to reject. The first answer, here or in the terminal, wins."
}

// DesktopEvidence is the proof the Desktop agent hands the hook for an owner
// ✅: the owner's signed reaction, the signed approval message it names, and
// the agent's grant that it read the message's history clean after the
// reaction.
type DesktopEvidence struct {
	Reaction nostr.Event  `json:"reaction"`
	Message  nostr.Event  `json:"message"`
	Grant    DesktopGrant `json:"grant"`
}

// DesktopGrant is the agent's BIP-340 signature over a domain-separated
// digest of the message id, the reaction id and the time it checked the
// history (desktopGrantDigest). It is not a Nostr event, so no relay or
// client can read it as one.
type DesktopGrant struct {
	CheckedAt int64  `json:"checked_at"`
	Sig       string `json:"sig"`
}

// DesktopCheck is what a Desktop allow must prove, all from the hook's own
// pin and rendering: the owner, the agent key and DM channel the hook pins,
// the hook's signed prompt, and the window the reaction and the grant must be
// dated in.
type DesktopCheck struct {
	Owner, Body, Channel string
	Prompt               string
	NotBefore, NotAfter  time.Time
}

// desktopGrantDigest is what a grant signs. The domain string keeps it apart
// from every Nostr event id and from the NIP-OA preimage.
func desktopGrantDigest(messageID, reactionID string, checkedAt int64) [32]byte {
	return sha256.Sum256([]byte("amq-remote/desktop-allow-grant/v1\x00" + messageID + "\x00" + reactionID + "\x00" + strconv.FormatInt(checkedAt, 10)))
}

// SignDesktopGrant signs the grant for one approval message and one owner
// reaction, checked at checkedAt, with the agent key. The caller must have
// read the message's history clean and found no owner ❌ first.
func SignDesktopGrant(secret [32]byte, messageID, reactionID string, checkedAt time.Time) (DesktopGrant, error) {
	priv, _ := btcec.PrivKeyFromBytes(secret[:])
	digest := desktopGrantDigest(messageID, reactionID, checkedAt.Unix())
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return DesktopGrant{}, err
	}
	return DesktopGrant{CheckedAt: checkedAt.Unix(), Sig: hex.EncodeToString(sig.Serialize())}, nil
}

// verifyDesktopGrant checks the grant's signature under the agent key body.
func verifyDesktopGrant(g DesktopGrant, body, messageID, reactionID string) bool {
	rawPub, err := hex.DecodeString(body)
	if err != nil {
		return false
	}
	pub, err := schnorr.ParsePubKey(rawPub)
	if err != nil {
		return false
	}
	rawSig, err := hex.DecodeString(g.Sig)
	if err != nil {
		return false
	}
	sig, err := schnorr.ParseSignature(rawSig)
	if err != nil {
		return false
	}
	digest := desktopGrantDigest(messageID, reactionID, g.CheckedAt)
	return sig.Verify(digest[:], pub)
}

// VerifyDesktopEvidence checks a Desktop owner ✅ as proof that the owner
// allowed the call want.Prompt shows. Every check must pass:
//
//   - the reaction is a kind 7 whose id and BIP-340 signature verify under
//     want.Owner, whose content is ✅ and whose last e tag is the message;
//   - the message is a kind 9 whose id and signature verify under want.Body,
//     posted in want.Channel (h tag), and whose content is exactly
//     DesktopApprovalText(want.Prompt);
//   - the grant verifies under want.Body over that message and reaction;
//   - the reaction and the grant are dated within [want.NotBefore,
//     want.NotAfter], and the grant no earlier than the reaction.
//
// The message's history is not in the evidence: the grant says the agent
// read it clean. Nothing in the evidence changes what must be proven.
func VerifyDesktopEvidence(raw json.RawMessage, want DesktopCheck) error {
	var ev DesktopEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	r, m, g := ev.Reaction, ev.Message, ev.Grant
	switch {
	case !validHexID(want.Owner) || !validHexID(want.Body) || want.Owner == want.Body || want.Channel == "":
		return errors.New("no complete pinned owner, agent and DM channel")
	case protocol.BoundPreview(want.Prompt) != want.Prompt:
		return errors.New("the call is not shown whole")
	case r.Kind != KindReaction:
		return errors.New("approve is not a reaction")
	case !r.CheckID() || !r.VerifySignature():
		return errors.New("reaction id or signature does not verify")
	case r.PubKey.Hex() != want.Owner:
		return errors.New("reaction is not signed by the pinned owner")
	case strings.TrimSpace(r.Content) != approveReaction:
		return errors.New("reaction is not the approve gesture")
	case lastETag(r) != m.ID.Hex():
		return errors.New("reaction does not target the approval message")
	case m.Kind != KindDM:
		return errors.New("approval message is not kind 9")
	case !m.CheckID() || !m.VerifySignature():
		return errors.New("approval message id or signature does not verify")
	case m.PubKey.Hex() != want.Body:
		return errors.New("approval message is not from the pinned agent")
	case tagValue(m, "h") != want.Channel:
		return errors.New("approval message is not in the pinned DM channel")
	case m.Content != DesktopApprovalText(want.Prompt):
		return errors.New("approval message does not show this call")
	case !verifyDesktopGrant(g, want.Body, m.ID.Hex(), r.ID.Hex()):
		return errors.New("the agent's grant does not verify")
	}
	reacted, checked := time.Unix(int64(r.CreatedAt), 0), time.Unix(g.CheckedAt, 0)
	switch {
	case reacted.Before(want.NotBefore) || reacted.After(want.NotAfter):
		return errors.New("reaction is outside the approval's window")
	case checked.Before(reacted) || checked.After(want.NotAfter):
		return errors.New("the agent's grant is outside the approval's window")
	}
	return nil
}
