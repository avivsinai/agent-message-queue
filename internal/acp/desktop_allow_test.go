package acp

import (
	"crypto/rand"
	"encoding/json"
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

// On Buzz Desktop with a Desktop pin, the agent posts the approval, reads the
// owner's signed ✅ and the message's history from its relay, and hands the
// endpoint an allow whose evidence the hook's Desktop verifier accepts.
func TestRemoteApprovalOwnerAllowFromTheDesktopDM(t *testing.T) {
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

	savedOwner, savedSelf, savedRelay, savedPost, savedPoll := buzzOwner, buzzSelf, buzzRelay, postAnswer, reactionPollInterval
	t.Cleanup(func() {
		buzzOwner, buzzSelf, buzzRelay, postAnswer, reactionPollInterval = savedOwner, savedSelf, savedRelay, savedPost, savedPoll
	})
	reactionPollInterval = 20 * time.Millisecond
	buzzOwner, buzzSelf = ownerHex, agentHex
	buzzRelay = &relay.Config{URL: url, Secret: agent, AuthTag: wire}
	var mu sync.Mutex
	var posts []string
	postAnswer = func(ch, content string, _ time.Duration) (string, error) {
		mu.Lock()
		posts = append(posts, content)
		mu.Unlock()
		msg := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Tags: nostr.Tags{{"h", ch}}, Content: content}
		if err := msg.Sign(agent); err != nil {
			return "", err
		}
		fakeRelay.Inject(msg)
		if content == buzzio.DesktopApprovalText(desktopAllowPrompt) {
			react := nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
			if err := react.Sign(owner); err != nil {
				return "", err
			}
			fakeRelay.Inject(react)
		}
		return msg.ID.Hex(), nil
	}

	rt := fake.New("fake", "e_1")
	var evidence []json.RawMessage
	s := remoteServer(t, claudeAllowApproval{Runtime: rt, mu: &mu, evidence: &evidence}, nil)
	s.cfg.StateDir = canonicalTempDir(t)
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
	for len(rt.Snapshot().Answers) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rt.Complete(id, "ran")
	<-done

	if answers := rt.Snapshot().Answers; len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "allow" {
		t.Fatalf("answers = %v, posts = %q; want one allow of i_1", answers, posts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evidence) != 1 {
		t.Fatalf("evidence = %d answers, want 1", len(evidence))
	}
	want := buzzio.DesktopCheck{Owner: ownerHex, Body: agentHex, Channel: channel, Prompt: desktopAllowPrompt,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute)}
	if err := buzzio.VerifyDesktopEvidence(evidence[0], want); err != nil {
		t.Fatalf("the hook's Desktop verifier refuses the evidence: %v", err)
	}
}
