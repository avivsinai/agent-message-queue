package acp

import (
	"crypto/rand"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
// it under a Desktop pin, once unbound is false. It records the evidence of
// each answer.
type claudeAllowApproval struct {
	*fake.Runtime
	mu       *sync.Mutex
	evidence *[]json.RawMessage
	unbound  *atomic.Bool // the attachment has not bound the tool call: deny only
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
			in.Kind, in.Prompt, in.RejectOption = "approval", desktopAllowPrompt, "deny"
			if !c.unbound.Load() {
				in.ApproveOption, in.ApproveProof = "allow", true
			}
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
// while the agent reads the message's edits. agentDeletion stores a deletion
// signed by the agent "before" the turn (buzz-acp removing an earlier turn's
// 💬) or "after" the approval is posted. With lateBind "✅" or "❌" the
// approval first offers deny only and offers allow once it is posted; the
// owner then reacts ✅ on the approval posted again, or ❌ on the first one.
// It returns once the runtime got an answer, or the owner was told ✅ cannot
// allow or the message was altered.
func runDesktopApproval(t *testing.T, pinned, denyMidway bool, agentDeletion, lateBind string) desktopRun {
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
		if lateBind != "" {
			return nil, nil // read from the relay, by the watcher that offers ✅
		}
		return []reaction{{Emoji: "✅", PubKeys: []string{ownerHex}}}, nil
	}
	sign := func(evt nostr.Event, key [32]byte) nostr.Event {
		if err := evt.Sign(key); err != nil {
			t.Error(err)
		}
		return evt
	}
	deletion := func() nostr.Event {
		return sign(nostr.Event{CreatedAt: nostr.Now(), Kind: buzzio.KindDeletion, Tags: nostr.Tags{{"e", strings.Repeat("7", 64)}}}, agent)
	}
	if agentDeletion == "before" {
		fakeRelay.Inject(deletion())
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
		if lateBind == "❌" && strings.Contains(content, denyOnlyText) {
			fakeRelay.Inject(sign(nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "❌", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}, owner))
		}
		if content == buzzio.DesktopApprovalText(desktopAllowPrompt) && lateBind != "❌" {
			if agentDeletion == "after" {
				fakeRelay.Inject(deletion())
			}
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
	unbound := &atomic.Bool{}
	unbound.Store(lateBind != "")
	s := remoteServer(t, claudeAllowApproval{Runtime: rt, mu: &mu, evidence: &run.evidence, unbound: unbound}, nil)
	s.cfg.StateDir = canonicalTempDir(t)
	// The turn ends when the test completes the request, not on a clock:
	// only the test binary's timeout bounds it.
	if deadline, bounded := t.Deadline(); bounded {
		s.cfg.TurnTimeout = time.Until(deadline)
	} else {
		s.cfg.TurnTimeout = time.Hour
	}
	eventID := strings.Repeat("d", 64)
	id, _ := remoteRequestID(eventID)
	turn := newTurn()
	turn.channel = channel
	done := make(chan struct{})
	var rpcErr *rpcError
	// Registered before the turn starts, so a failed wait still ends the
	// turn and joins it.
	t.Cleanup(func() {
		s.mu.Lock()
		if turn.settleLocked("client_disconnected") {
			close(turn.done)
		}
		s.mu.Unlock()
		<-done
	})
	go func() {
		defer close(done)
		_, rpcErr = s.runRemote("s", "run the tests", eventID, turn, func(any) error { return nil })
	}()
	// ended reports that the turn returned early, and fails with its error.
	ended := func() bool {
		select {
		case <-done:
			t.Fatalf("the turn ended before the approval was answered: %v", rpcErr)
			return true
		default:
			return false
		}
	}
	waitFor(t, "the request to run", func() bool { return ended() || rt.HasRun(id) })
	rt.Question(id, "i_1", []string{"allow", "deny"})
	if lateBind != "" {
		waitFor(t, "the deny-only approval", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return ended() || len(run.posts) > 0
		})
		unbound.Store(false)
		rt.Question(id, "i_1", []string{"allow", "deny"})
	}
	waitFor(t, "an answer, or the owner told ✅ cannot allow", func() bool {
		if ended() {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		return len(rt.Snapshot().Answers) > 0 || slices.Contains(run.posts, allowRefusedText) || slices.Contains(run.posts, alteredText)
	})
	rt.Complete(id, "ran")
	<-done
	run.answers = rt.Snapshot().Answers
	return run
}

// On Buzz Desktop with a Desktop pin, the agent posts the approval, reads the
// owner's signed ✅ and the message's history from its relay, and hands the
// endpoint an allow whose evidence the hook's Desktop verifier accepts.
func TestRemoteApprovalOwnerAllowFromTheDesktopDM(t *testing.T) {
	run := runDesktopApproval(t, true, false, "", "")
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
	run := runDesktopApproval(t, true, true, "", "")
	if len(run.answers) != 1 || run.answers[0].Option != "deny" {
		t.Fatalf("answers = %v; want the ❌ stored mid-check to deny", run.answers)
	}
}

// Review of #1179 (P2): an approval that offers allow for another pin (a
// relay share, another Desktop DM) was posted with ✅, which the hook then
// refused. It stays deny-only and shows the command that pins this DM.
func TestRemoteApprovalWithoutThisDesktopPinStaysDenyOnly(t *testing.T) {
	run := runDesktopApproval(t, false, false, "", "")
	if len(run.answers) != 0 {
		t.Fatalf("answers = %v; want no allow without this DM's pin", run.answers)
	}
	if len(run.posts) == 0 || !strings.Contains(run.posts[0], denyOnlyText) || !strings.Contains(run.posts[0], "install-approval-hook --desktop") ||
		!slices.Contains(run.posts, allowRefusedText) {
		t.Fatalf("posts = %q; want the deny-only approval with the pin command, then the ✅ refused", run.posts)
	}
}

// Live check on Buzz Desktop 0.5.28 (bead 611.42.12.1): buzz-acp deletes its
// own 💬 with the agent key after each turn, and every later ✅ was refused as
// altered. A deletion by the agent stored before the approval was posted
// allows; one stored after it still refuses.
func TestRemoteApprovalDesktopAllowAfterAnEarlierTurnsReactionCleanup(t *testing.T) {
	run := runDesktopApproval(t, true, false, "before", "")
	if len(run.answers) != 1 || run.answers[0].Option != "allow" {
		t.Fatalf("answers = %v, posts = %q; want the ✅ to allow after an earlier deletion by the agent", run.answers, run.posts)
	}
	run = runDesktopApproval(t, true, false, "after", "")
	if len(run.answers) != 0 || !slices.Contains(run.posts, alteredText) {
		t.Fatalf("answers = %v, posts = %q; want a deletion by the agent after the post to refuse as altered", run.answers, run.posts)
	}
}

// Live check on Buzz Desktop (bead 611.42.12.3): the hook opened the request
// in the same second Claude wrote the tool call, so the endpoint published
// the approval deny-only and offered allow only after it bound the call.
// amq-acp kept the first version and the owner never saw ✅. The approval is
// posted again with ✅, and a ❌ on the first message still denies.
func TestRemoteApprovalDesktopAllowWhenTheCallBindsLate(t *testing.T) {
	run := runDesktopApproval(t, true, false, "", "✅")
	if len(run.answers) != 1 || run.answers[0].Option != "allow" {
		t.Fatalf("answers = %v, posts = %q; want the approval posted again with ✅ and the ✅ to allow", run.answers, run.posts)
	}
	run = runDesktopApproval(t, true, false, "", "❌")
	if len(run.answers) != 1 || run.answers[0].Option != "deny" {
		t.Fatalf("answers = %v, posts = %q; want the ❌ on the first, deny-only message to deny", run.answers, run.posts)
	}
}

// waitFor polls cond until it holds. Only the test binary's timeout bounds
// the wait (t.Deadline); a wait that never ends fails with what it waited
// for, shortly before that timeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline, bounded := t.Deadline()
	for !cond() {
		if bounded && time.Until(deadline) < 5*time.Second {
			t.Fatalf("still waiting for %s when the test binary's timeout came near", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
