package acp

import (
	"crypto/rand"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// desktopAllowPrompt is a signed prompt the Claude hook renders for a call
// shown whole.
const desktopAllowPrompt = "Bash command:\ngo test ./...\n\nInteraction: cc-0123456789abcdef0123456789abcdef\nAction: sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// claudeAllowApproval is the fake runtime presented as a Claude session
// whose approval offers allow with proof, as the Claude attachment projects
// it under a Desktop pin. It records the evidence of each answer.
type claudeAllowApproval struct {
	*fake.Runtime
	mu       *sync.Mutex
	evidence *[]json.RawMessage
}

func (c claudeAllowApproval) Inspect() protocol.Session {
	s := c.Runtime.Inspect()
	s.Harness = "claude_code"
	return s
}

func (c claudeAllowApproval) Subscribe(fn func(core.NativeEvent)) func() {
	return c.Runtime.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventQuestion && ev.Interaction != nil {
			in := *ev.Interaction
			in.Kind, in.Prompt, in.ApproveOption, in.RejectOption, in.ApproveProof = "approval", desktopAllowPrompt, "allow", "deny", true
			ev.Interaction = &in
		}
		fn(ev)
	})
}

func (c claudeAllowApproval) RespondWithEvidence(key requests.Key, epoch, interactionID, option string, evidence json.RawMessage) (protocol.Code, error) {
	c.mu.Lock()
	*c.evidence = append(*c.evidence, evidence)
	c.mu.Unlock()
	return c.Respond(key, epoch, interactionID, option)
}

// desktopRun is what one Desktop approval left behind: the runtime's
// answers, every post, and the evidence of each answer.
type desktopRun struct {
	answers  []fake.Answer
	posts    []string
	evidence []json.RawMessage
}

// runDesktopApproval drives one native turn whose Claude approval offers
// allow with proof. The owner reacts ✅ on the posted approval. With pinned
// false the hook pins another DM. With denyMidway the owner's ❌ is stored
// while the agent reads the message's edits. It returns once the runtime got
// an answer, or the owner was told ✅ cannot allow.
func runDesktopApproval(t *testing.T, pinned, denyMidway bool) desktopRun {
	t.Helper()
	const channel = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	var owner, agent [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(agent[:])
	ownerHex, agentHex := nostr.GetPublicKey(owner).Hex(), nostr.GetPublicKey(agent).Hex()
	tag, err := bodykey.SignAuthTag(owner, agentHex, bodykey.ShareConditions(relay.KindAuth, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	wire := []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
	fakeRelay, srv, url := relaytest.Start(agentHex, wire)
	t.Cleanup(srv.Close)

	savedOwner, savedSelf, savedRelay, savedPost, savedRead, savedPinned, savedPoll := buzzOwner, buzzSelf, buzzRelay, postAnswer, readReactions, desktopPinned, reactionPollInterval
	t.Cleanup(func() {
		buzzOwner, buzzSelf, buzzRelay, postAnswer, readReactions, desktopPinned, reactionPollInterval = savedOwner, savedSelf, savedRelay, savedPost, savedRead, savedPinned, savedPoll
	})
	reactionPollInterval = 20 * time.Millisecond
	buzzOwner, buzzSelf = ownerHex, agentHex
	buzzRelay = &relay.Config{URL: url, Secret: agent, AuthTag: wire}
	desktopPinned = func(o, b, c string) bool { return pinned && o == ownerHex && b == agentHex && c == channel }
	readReactions = func(string, time.Duration) ([]reaction, error) {
		return []reaction{{Emoji: "✅", PubKeys: []string{ownerHex}}}, nil
	}
	sign := func(evt nostr.Event, key [32]byte) nostr.Event {
		if err := evt.Sign(key); err != nil {
			t.Error(err)
		}
		return evt
	}
	var mu sync.Mutex
	var run desktopRun
	approval := ""
	postAnswer = func(ch, content string, _ time.Duration) (string, error) {
		msg := sign(nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Tags: nostr.Tags{{"h", ch}}, Content: content}, agent)
		fakeRelay.Inject(msg)
		mu.Lock()
		run.posts = append(run.posts, content)
		if strings.Contains(content, "Approval needed") {
			approval = msg.ID.Hex()
		}
		mu.Unlock()
		if content == buzzio.DesktopApprovalText(desktopAllowPrompt) {
			fakeRelay.Inject(sign(nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}, owner))
		}
		return msg.ID.Hex(), nil
	}
	var once sync.Once
	fakeRelay.BeforeReq = func(filters []nostr.Filter) {
		if !denyMidway || len(filters) != 1 || !slices.Contains(filters[0].Kinds, nostr.Kind(buzzio.KindEdit)) {
			return
		}
		once.Do(func() {
			mu.Lock()
			id := approval
			mu.Unlock()
			fakeRelay.Inject(sign(nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "❌", Tags: nostr.Tags{{"e", id}}}, owner))
		})
	}

	rt := fake.New("fake", "e_1")
	s := remoteServer(t, claudeAllowApproval{Runtime: rt, mu: &mu, evidence: &run.evidence}, nil)
	s.cfg.StateDir = canonicalTempDir(t)
	// Relay sign-in and five reads run inside the turn; a loaded machine
	// must not end the turn first.
	s.cfg.TurnTimeout = 4 * time.Second
	eventID := strings.Repeat("d", 64)
	id, _ := remoteRequestID(eventID)
	turn := newTurn()
	turn.channel = channel
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.runRemote("s", "run the tests", eventID, turn, func(any) error { return nil })
	}()
	deadline := time.Now().Add(4 * time.Second)
	for !rt.HasRun(id) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rt.Question(id, "i_1", []string{"allow", "deny"})
	settled := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(rt.Snapshot().Answers) > 0 || slices.Contains(run.posts, allowRefusedText)
	}
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rt.Complete(id, "ran")
	<-done
	run.answers = rt.Snapshot().Answers
	return run
}

// On Buzz Desktop with a Desktop pin, the agent posts the approval, reads the
// owner's signed ✅ and the message's history from its relay, and hands the
// endpoint an allow whose evidence the hook's Desktop verifier accepts.
func TestRemoteApprovalOwnerAllowFromTheDesktopDM(t *testing.T) {
	run := runDesktopApproval(t, true, false)
	if len(run.answers) != 1 || run.answers[0].InteractionID != "i_1" || run.answers[0].Option != "allow" {
		t.Fatalf("answers = %v, posts = %q; want one allow of i_1", run.answers, run.posts)
	}
	if len(run.evidence) != 1 {
		t.Fatalf("evidence = %d answers, want 1", len(run.evidence))
	}
	want := buzzio.DesktopCheck{Owner: buzzOwner, Body: buzzSelf, Channel: "6eff60e4-32ab-48ec-bd3d-f4c97872f370", Prompt: desktopAllowPrompt,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute)}
	if err := buzzio.VerifyDesktopEvidence(run.evidence[0], want); err != nil {
		t.Fatalf("the hook's Desktop verifier refuses the evidence: %v", err)
	}
}

// Review of #1179 (P1): the owner's ❌ stored while the agent read the
// message's history was missed and the ✅ was allowed. Deny beats allow up
// to the final reaction read before the grant.
func TestRemoteApprovalOwnerDenyDuringHistoryBeatsAllow(t *testing.T) {
	run := runDesktopApproval(t, true, true)
	if len(run.answers) != 1 || run.answers[0].Option != "deny" {
		t.Fatalf("answers = %v; want the ❌ stored mid-check to deny", run.answers)
	}
}

// Review of #1179 (P2): an approval that offers allow for another pin (a
// relay share, another Desktop DM) was posted with ✅, which the hook then
// refused. It stays deny-only and shows the command that pins this DM.
func TestRemoteApprovalWithoutThisDesktopPinStaysDenyOnly(t *testing.T) {
	run := runDesktopApproval(t, false, false)
	if len(run.answers) != 0 {
		t.Fatalf("answers = %v; want no allow without this DM's pin", run.answers)
	}
	if len(run.posts) == 0 || !strings.Contains(run.posts[0], denyOnlyText) || !strings.Contains(run.posts[0], "install-approval-hook --desktop") ||
		!slices.Contains(run.posts, allowRefusedText) {
		t.Fatalf("posts = %q; want the deny-only approval with the pin command, then the ✅ refused", run.posts)
	}
}
