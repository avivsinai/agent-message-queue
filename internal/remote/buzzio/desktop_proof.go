package buzzio

import (
	"context"
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

	"github.com/avivsinai/agent-message-queue/internal/relay"
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
//     want.NotAfter]. The reaction's date is the owner's clock and the
//     grant's is this machine's, so neither is ordered against the other.
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
	case checked.Before(want.NotBefore) || checked.After(want.NotAfter):
		return errors.New("the agent's grant is outside the approval's window")
	}
	return nil
}

// ReadEvent reads the stored event with id from the relay, up to the end of
// stored events. found is false when the relay holds none.
func ReadEvent(ctx context.Context, conn *relay.Conn, id string) (evt nostr.Event, found bool, err error) {
	eid, err := nostr.IDFromHex(id)
	if err != nil {
		return nostr.Event{}, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	got, err := readStored(ctx, conn, nostr.Filter{IDs: []nostr.ID{eid}})
	if err != nil || len(got) == 0 {
		return nostr.Event{}, false, err
	}
	return got[0], true, nil
}

// OwnerReactions reads the owner's stored reactions whose last e tag is the
// message, oldest first as the relay returns them.
func OwnerReactions(ctx context.Context, conn *relay.Conn, messageID, owner string) ([]nostr.Event, error) {
	pk, err := nostr.PubKeyFromHex(owner)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	got, err := readStored(ctx, conn, nostr.Filter{Kinds: []nostr.Kind{KindReaction}, Authors: []nostr.PubKey{pk}, Tags: nostr.TagMap{"e": {messageID}}})
	if err != nil {
		return nil, err
	}
	out := got[:0]
	for _, r := range got {
		if lastETag(r) == messageID {
			out = append(out, r)
		}
	}
	return out, nil
}

// relayDriftWindow is how far from its own clock the Buzz relay accepts an
// event's created_at (ingest MAX_TIMESTAMP_DRIFT_SECS). An event stored after
// a moment T is therefore dated no earlier than T minus this window.
const relayDriftWindow = 900 * time.Second

// maxAgentDeletions bounds one read of the agent's deletions in the drift
// window. A full page may hide one, so it is an unknown history.
const maxAgentDeletions = 100

// AgentDeletions is the agent's deletions stored just before an approval was
// posted: every one dated at or after Since. buzz-acp signs with the agent
// key and deletes its own 👀 and 💬 reactions after each turn, with a
// deletion that names only the reaction's id, and the relay hides what a
// deletion names from every read. So an older deletion by the agent cannot
// be told from one that hid an edit; only a deletion stored after the
// message was posted can have hidden an edit of it.
type AgentDeletions struct {
	Since nostr.Timestamp
	IDs   map[nostr.ID]bool
}

// ReadAgentDeletions reads the agent's deletions dated within the relay's
// drift window before now. Read before the approval message is posted, it
// holds no deletion that can name the message or an edit of it.
func ReadAgentDeletions(ctx context.Context, conn *relay.Conn, agent string, now time.Time) (AgentDeletions, error) {
	pk, err := nostr.PubKeyFromHex(agent)
	if err != nil {
		return AgentDeletions{}, fmt.Errorf("agent pubkey: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	since := nostr.Timestamp(now.Add(-relayDriftWindow).Unix())
	got, err := readAgentDeletions(ctx, conn, pk, since)
	if err != nil {
		return AgentDeletions{}, err
	}
	ids := make(map[nostr.ID]bool, len(got))
	for _, d := range got {
		ids[d.ID] = true
	}
	return AgentDeletions{Since: since, IDs: ids}, nil
}

func readAgentDeletions(ctx context.Context, conn *relay.Conn, agent nostr.PubKey, since nostr.Timestamp) ([]nostr.Event, error) {
	got, err := readStored(ctx, conn, nostr.Filter{Kinds: []nostr.Kind{KindDeletion, KindGroupDeletion}, Authors: []nostr.PubKey{agent}, Since: since, Limit: maxAgentDeletions})
	if err != nil {
		return nil, fmt.Errorf("read the agent's deletions: %w", err)
	}
	if len(got) >= maxAgentDeletions {
		return nil, fmt.Errorf("the agent has %d or more deletions since %d; its history is not known", maxAgentDeletions, since)
	}
	return got, nil
}

// CheckDesktopHistory reads a Desktop approval message's history from the
// relay, within historyTimeout. The Desktop agent never edits or deletes an
// approval message, so any edit naming it, any deletion naming it, any
// deletion by the owner since the message, and any deletion by the agent
// that was not stored before the message was posted (before) is ErrAltered.
// An edit can name the message only after it was posted, so a deletion that
// hid one was stored after before was read, and the relay dates it no
// earlier than before.Since. A read that errors, times out or fills its
// page is another error.
func CheckDesktopHistory(ctx context.Context, conn *relay.Conn, msg nostr.Event, owner string, before AgentDeletions) error {
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	pk, err := nostr.PubKeyFromHex(owner)
	if err != nil {
		return fmt.Errorf("owner pubkey: %w", err)
	}
	deletionKinds := []nostr.Kind{KindDeletion, KindGroupDeletion}
	reads := []struct {
		what   string
		filter nostr.Filter
	}{
		{"an edit of the message", nostr.Filter{Kinds: []nostr.Kind{KindEdit}, Tags: nostr.TagMap{"e": {msg.ID.Hex()}}, Limit: 1}},
		{"a deletion of the message", nostr.Filter{Kinds: deletionKinds, Tags: nostr.TagMap{"e": {msg.ID.Hex()}}, Limit: 1}},
		{"a deletion by the owner since the message", nostr.Filter{Kinds: deletionKinds, Authors: []nostr.PubKey{pk}, Since: msg.CreatedAt, Limit: 1}},
	}
	for _, r := range reads {
		got, err := readStored(ctx, conn, r.filter)
		if err != nil {
			return fmt.Errorf("read %s: %w", r.what, err)
		}
		if len(got) > 0 {
			return fmt.Errorf("%w: %s", ErrAltered, r.what)
		}
	}
	got, err := readAgentDeletions(ctx, conn, msg.PubKey, before.Since)
	if err != nil {
		return err
	}
	for _, d := range got {
		if !before.IDs[d.ID] {
			return fmt.Errorf("%w: a deletion by the agent since the message was posted", ErrAltered)
		}
	}
	return nil
}
