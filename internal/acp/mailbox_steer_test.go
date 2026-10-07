package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// runSteeredTurn runs one mailbox turn of session sid for event and calls
// steer once the prompt is in the handle's inbox in root, while the turn is
// still open. Then it answers the prompt and returns the turn's result.
func runSteeredTurn(t *testing.T, s *Server, sid, event, root string, steer func(prompt string)) remotePromptResult {
	t.Helper()
	thread := cockpitThread("session/" + sid)
	turn := newTurn()
	s.mu.Lock()
	session := s.sessions[sid]
	if session == nil {
		session = &sessionState{ID: sid, Thread: thread}
		s.sessions[sid] = session
	}
	session.turn = turn
	s.mu.Unlock()
	before := inboxPrompts(t, root)
	done := false
	result, rpcErr := s.runRemote(sid, "build the thing", event, turn, func(v any) error {
		if done || !strings.HasPrefix(v.(sessionUpdateNotification).Params.Update.Content.Text, "Delivered to") {
			return nil
		}
		done = true
		prompt := slices.DeleteFunc(inboxPrompts(t, root), func(id string) bool { return slices.Contains(before, id) })
		if len(prompt) != 1 {
			t.Fatalf("new prompts in %s: %v", root, prompt)
		}
		steer(prompt[0])
		replyAs(t, root, thread, prompt[0], format.KindAnswer, "done")
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	return result.(remotePromptResult)
}

// steerEvent sends a follow-up DM as _session/steering for a Nostr event.
func steerEvent(s *Server, sid, text, event string) response {
	resp, _ := s.handle([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"_session/steering","params":{"sessionId":%q,"prompt":%q,"_meta":{"nostr":{"eventId":%q}}}}`, sid, text, event)))
	return resp
}

// Bead agent-message-queue-cfy: a follow-up DM during an open mailbox turn
// reaches the bound handle as an urgent steer from buzz on the turn's thread
// that refs the prompt, and the turn still ends on the final reply.
func TestFollowUpDMSteersAnOpenMailboxTurn(t *testing.T) {
	s, root := mailboxServer(t)
	var prompt string
	var steered steeringResult
	result := runSteeredTurn(t, s, "s", strings.Repeat("1", 64), root, func(p string) {
		resp, _ := s.handle([]byte(steeringRequest(7, "s", "use the smaller design")))
		if resp.Error != nil {
			t.Fatalf("steering refused: %+v", resp.Error)
		}
		prompt, steered = p, resp.Result.(steeringResult)
	})
	if result.StopReason != StopReasonEndTurn || steered.Outcome != SteeringInjected {
		t.Fatalf("result = %+v, steering = %+v; want end_turn and %s", result, steered, SteeringInjected)
	}
	steer, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "agent"), steered.Meta.AMQ.MessageID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	thread := cockpitThread("session/s")
	if h := steer.Header; h.From != mailboxSender || h.Thread != thread || h.Priority != format.PriorityUrgent || !slices.Contains(h.Refs, prompt) {
		t.Fatalf("steer header = %+v, want urgent from buzz on %s with refs to %s", h, thread, prompt)
	}
}

// Review of #965, finding 1: the steer journal was root-local, so a steer
// event replayed after set_model chose another binding was delivered again
// into that binding's turn.
func TestReplayedSteerStaysWithItsFirstBinding(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	rootA, rootB := canonicalTempDir(t), canonicalTempDir(t)
	for _, root := range []string{rootA, rootB} {
		if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []binding.Binding{
		{Carrier: binding.CarrierMailbox, Root: rootA, Handle: "agent", Name: "a"},
		{Carrier: binding.CarrierMailbox, Root: rootB, Handle: "agent", Name: "b"},
	} {
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(Config{RemoteBinding: true, StateDir: canonicalTempDir(t), TurnTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond}, "test")
	s.sessions["s"] = &sessionState{ID: "s", Thread: cockpitThread("session/s"), binding: "a"}
	steerID := strings.Repeat("e", 64)
	runSteeredTurn(t, s, "s", strings.Repeat("1", 64), rootA, func(string) {
		if resp := steerEvent(s, "s", "smaller design", steerID); resp.Error != nil || resp.Result.(steeringResult).Outcome != SteeringInjected {
			t.Fatalf("first steer = %+v", resp)
		}
	})
	s.sessions["s"].binding = "b"
	var replay response
	runSteeredTurn(t, s, "s", strings.Repeat("2", 64), rootB, func(string) {
		replay = steerEvent(s, "s", "smaller design", steerID)
	})
	if replay.Error != nil || replay.Result.(steeringResult).Outcome != SteeringDuplicate {
		t.Fatalf("replayed steer = %+v, want %s", replay, SteeringDuplicate)
	}
	if n := len(inboxPrompts(t, rootB)); n != 1 {
		t.Fatalf("binding b inbox holds %d messages; want only its prompt", n)
	}
}

// Review of #965, finding 2: a steer was written after another process had
// recorded the event's final answer.
func TestSteerAfterTheFinalAnswerIsRefused(t *testing.T) {
	s, root := mailboxServer(t)
	event := strings.Repeat("1", 64)
	var resp response
	runSteeredTurn(t, s, "s", event, root, func(string) {
		marker := filepath.Join(s.cfg.StateDir, "remote-events", event+".posted."+postFinal)
		if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		resp = steerEvent(s, "s", "smaller design", strings.Repeat("e", 64))
	})
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("steer after the final = %+v, want a method-not-found refusal", resp)
	}
	if n := len(inboxPrompts(t, root)); n != 1 {
		t.Fatalf("inbox holds %d messages; want only the prompt", n)
	}
}

// Review of #965, finding 3: a steer committed to the inbox was reported as
// failed when the event journal write failed afterwards.
func TestCommittedSteerIsADeliveryWithoutTheJournal(t *testing.T) {
	s, root := mailboxServer(t)
	var resp response
	runSteeredTurn(t, s, "s", strings.Repeat("1", 64), root, func(string) {
		journal := filepath.Join(root, "agents", mailboxSender, "outbox", "acp-events")
		if err := os.MkdirAll(journal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(journal, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(journal, 0o700) })
		resp = steerEvent(s, "s", "smaller design", strings.Repeat("e", 64))
	})
	if resp.Error != nil {
		t.Fatalf("committed steer reported %+v", resp.Error)
	}
	if n := len(inboxPrompts(t, root)); n != 2 {
		t.Fatalf("inbox holds %d messages; want the prompt and one steer", n)
	}
}
