package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/secretscan"
)

// A native Claude turn driven from Buzz Desktop has no relay share, so the
// owner's reaction cannot carry the signed proof an allow needs. amq-acp
// shows each pending tool approval in the DM and relays only the owner's ❌
// as a deny; allow stays in Claude's terminal (bead
// agent-message-queue-611.42.2; allow from Desktop is 611.42.12).

// denyOnlyText follows every approval this path posts.
const denyOnlyText = "Buzz can deny this request only. ❌ denies; to allow, answer in Claude's terminal. ✅ here cannot approve."

// allowRefusedText answers an owner's ✅, once per approval.
const allowRefusedText = "Allow is not available from Buzz here; answer in Claude's terminal."

// deniedText says the owner's ❌ blocked the tool call.
const deniedText = "Denied from Buzz: the tool call was blocked."

// reactionPollInterval is the gap between reads of the owner's reactions on
// a posted approval: every few seconds while it is pending.
const reactionPollInterval = 3 * time.Second

// buzzOwner is the managed agent's owner pubkey (64 lowercase hex), or ""
// when it cannot be established. Only its reactions answer an approval.
var buzzOwner string

// agentOwner is the owner that Buzz Desktop gave this agent: the NIP-OA
// BUZZ_AUTH_TAG ["auth", owner, conditions, sig], verified against the
// agent's own key as buzz-acp verifies it (resolve_agent_owner), else
// BUZZ_ACP_AGENT_OWNER, which Desktop sets only for an agent with no auth
// tag. A tag that does not verify yields no owner.
func agentOwner(authTag, secret, fallback string) string {
	if authTag = strings.TrimSpace(authTag); authTag != "" {
		var tag []string
		if json.Unmarshal([]byte(authTag), &tag) != nil {
			return ""
		}
		t, err := bodykey.ParseAuthTag(tag)
		if err != nil {
			return ""
		}
		agent, err := agentPubKey(strings.TrimSpace(secret))
		if err != nil || t.Verify(agent) != nil {
			return ""
		}
		return t.OwnerPubKey
	}
	if fallback = strings.ToLower(strings.TrimSpace(fallback)); len(fallback) == 64 {
		if _, err := nostr.PubKeyFromHex(fallback); err == nil {
			return fallback
		}
	}
	return ""
}

// agentPubKey is the hex public key of a hex or nsec secret.
func agentPubKey(secret string) (string, error) {
	if strings.HasPrefix(secret, "nsec1") {
		prefix, val, err := nip19.Decode(secret)
		sk, ok := val.(nostr.SecretKey)
		if err != nil || prefix != "nsec" || !ok {
			return "", errors.New("invalid nsec")
		}
		return sk.Public().Hex(), nil
	}
	sk, err := nostr.SecretKeyFromHex(secret)
	if err != nil {
		return "", err
	}
	return sk.Public().Hex(), nil
}

// reaction is one emoji on a message and who reacted with it.
type reaction struct {
	Emoji   string   `json:"emoji"`
	PubKeys []string `json:"pubkeys"`
}

// readReactions reads the reactions on one event. A variable so tests can
// supply reactions without a relay.
var readReactions = reactionsWithBuzzCLI

// reactionsWithBuzzCLI runs `buzz reactions get --event <id>`, which prints
// {"reactions":[{"emoji","count","pubkeys"}]}.
func reactionsWithBuzzCLI(eventID string, budget time.Duration) ([]reaction, error) {
	out, err := runBuzzCLI(budget, nil, "reactions", "get", "--event", eventID)
	if err != nil {
		return nil, err
	}
	var got struct {
		Reactions []reaction `json:"reactions"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("buzz reactions get: %v", err)
	}
	return got.Reactions, nil
}

// reacted reports whether owner reacted with emoji.
func reacted(rs []reaction, owner, emoji string) bool {
	for _, r := range rs {
		if strings.TrimSpace(r.Emoji) == emoji && slices.Contains(r.PubKeys, owner) {
			return true
		}
	}
	return false
}

// approvalText is the DM message for one pending approval: the bounded
// preview, or "Command hidden" when it may hold a secret, then the
// deny-only note.
func approvalText(prompt string) string {
	preview := protocol.BoundPreview(prompt)
	if secretscan.MayHold(preview) {
		preview = "Command hidden"
	}
	return "Approval needed:\n\n" + preview + "\n\n" + denyOnlyText
}

// denyApproval reports whether in is an approval this path can show: a
// remote answer with a reject option.
func denyApproval(in *protocol.Interaction) bool {
	return in != nil && in.Kind == "approval" && in.RemoteAnswer && in.RejectOption != ""
}

// approvals follows the pending approvals of one remote turn. The follow
// loop calls observe with every snapshot; each new approval gets one
// watcher, which posts it once and polls the owner's reactions until the
// approval resolves or the turn ends.
type approvals struct {
	r       *remoteTurn
	channel string
	owner   string

	mu       sync.Mutex
	stops    map[string]chan struct{} // interaction id -> its watcher's stop
	denied   map[string]string        // interaction id -> reject option this turn sent
	reported map[string]bool          // denials whose outcome was posted
	wg       sync.WaitGroup
}

func newApprovals(r *remoteTurn, channel, owner string) *approvals {
	return &approvals{r: r, channel: channel, owner: owner, stops: map[string]chan struct{}{}, denied: map[string]string{}, reported: map[string]bool{}}
}

// observe starts a watcher for a new approval, retires watchers of
// approvals no longer pending, and reports a deny the record shows applied.
func (a *approvals) observe(snap protocol.Snapshot) {
	if a.channel == "" || a.owner == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := ""
	if in := snap.Interaction; denyApproval(in) {
		current = in.InteractionID
		if _, seen := a.stops[current]; !seen {
			stop := make(chan struct{})
			a.stops[current] = stop
			a.wg.Add(1)
			go a.watch(snap, *in, stop)
		}
	}
	for id, stop := range a.stops {
		if id != current && stop != nil {
			close(stop)
			a.stops[id] = nil
		}
	}
	a.reportLocked(snap)
}

// reportLocked posts the outcome line once for each deny of this turn that
// the record shows answered with the reject option.
func (a *approvals) reportLocked(snap protocol.Snapshot) {
	for _, res := range snap.Resolved {
		option, sent := a.denied[res.InteractionID]
		if sent && !a.reported[res.InteractionID] && res.Outcome == protocol.ResolutionAnswered && res.Option == option {
			a.reported[res.InteractionID] = true
			a.r.s.publish(a.channel, deniedText)
		}
	}
}

// close stops every watcher and waits for them.
func (a *approvals) close() {
	a.mu.Lock()
	for id, stop := range a.stops {
		if stop != nil {
			close(stop)
			a.stops[id] = nil
		}
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// watch posts one approval and relays the owner's ❌ as its reject option.
func (a *approvals) watch(snap protocol.Snapshot, in protocol.Interaction, stop chan struct{}) {
	defer a.wg.Done()
	budget := a.r.s.cfg.PostTimeout
	posted, err := postAnswer(a.channel, approvalText(in.Prompt), budget)
	if err != nil || posted == "" {
		return
	}
	allowAnswered := false
	for {
		select {
		case <-stop:
			return
		default:
		}
		if rs, err := readReactions(posted, budget); err == nil {
			if reacted(rs, a.owner, "❌") {
				a.deny(snap, in)
				return
			}
			if !allowAnswered && reacted(rs, a.owner, "✅") {
				allowAnswered = true
				a.r.s.publish(a.channel, allowRefusedText)
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(reactionPollInterval):
		}
	}
}

// deny answers the approval with its reject option. already_resolved means
// another answer came first: not an error, and nothing to report.
func (a *approvals) deny(snap protocol.Snapshot, in protocol.Interaction) {
	rep, err := a.r.call(&protocol.Command{
		Schema:        protocol.SchemaCommand,
		Op:            protocol.OpInteractionRespond,
		RequestRef:    snap.RequestRef,
		TargetID:      snap.TargetID,
		Epoch:         snap.Epoch,
		InteractionID: in.InteractionID,
		Option:        in.RejectOption,
	})
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.denied[in.InteractionID] = in.RejectOption
	a.reportLocked(rep.Snapshot)
}
