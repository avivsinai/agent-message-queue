package acp

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

func steeringRequest(id int, sessionID, text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"_session/steering","params":{"sessionId":%q,"prompt":%s}}`, id, sessionID, encoded)
}

// ebo PR2: steering during an in-flight turn is injected as an urgent
// buzz-steer message on the cockpit thread, and the turn still completes
// with the peer's reply.
func TestSteeringDuringTurnIsInjectedUrgent(t *testing.T) {
	cfg := testConfig(t)
	// The turn must stay open across the steer and the reply; the 40ms
	// package default expired first under load (cursor's pre-push run).
	cfg.TurnTimeout = 5 * time.Second
	live, sessionID, threadID := newLiveSession(t, cfg, "steer-live")
	live.send(promptRequest(3, sessionID, "build the thing"))
	live.readUntilUpdate("agent_thought_chunk")

	live.send(steeringRequest(4, sessionID, "use the smaller design"))
	result := live.readUntilResult()["result"].(map[string]any)
	if result["outcome"] != SteeringInjected {
		t.Fatalf("steering outcome = %v, want %s", result["outcome"], SteeringInjected)
	}
	steer := readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitSteeringSubject)
	if steer.Header.Thread != threadID || steer.Header.Priority != format.PriorityUrgent || !slices.Contains(steer.Header.Labels, "buzz-steer") {
		t.Fatalf("steering header = %+v, want urgent buzz-steer on %s", steer.Header, threadID)
	}

	deliverReply(t, cfg, threadID, "done, smaller design")
	if stop := live.readUntilResult()["result"].(map[string]any)["stopReason"]; stop != StopReasonEndTurn {
		t.Fatalf("prompt stopReason = %v, want %s", stop, StopReasonEndTurn)
	}
}

// ebo PR2: steering with no turn in flight is an ordinary normal-priority
// message that starts a new turn on the peer.
func TestSteeringWhileIdleStartsNewTurn(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, threadID := newLiveSession(t, cfg, "steer-idle")
	live.send(steeringRequest(3, sessionID, "also check the docs"))
	result := live.readUntilResult()["result"].(map[string]any)
	if result["outcome"] != SteeringStartedNewTurn {
		t.Fatalf("steering outcome = %v, want %s", result["outcome"], SteeringStartedNewTurn)
	}
	steer := readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitSteeringSubject)
	if steer.Header.Thread != threadID || steer.Header.Priority != format.PriorityNormal || slices.Contains(steer.Header.Labels, "buzz-steer") {
		t.Fatalf("steering header = %+v, want normal priority without buzz-steer", steer.Header)
	}
}

// ebo PR2: session/cancel (an ACP notification) ends the in-flight turn
// with the typed cancelled stop; the queued prompt stays in the inbox.
func TestCancelEndsInFlightTurnAsCancelled(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = 5 * time.Second // the cancel, not the timeout, must end the turn
	live, sessionID, _ := newLiveSession(t, cfg, "cancel-live")
	live.send(promptRequest(3, sessionID, "long task"))
	live.readUntilUpdate("agent_thought_chunk")
	readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitPromptSubject)

	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, sessionID))
	result := live.readUntilResult()["result"].(map[string]any)
	meta := result["_meta"].(map[string]any)["amq"].(map[string]any)
	if result["stopReason"] != StopReasonCancelled || meta["state"] != DeliveryStateCancelled || meta["reason"] != "session_cancelled" {
		t.Fatalf("cancelled turn = %v, want stopReason cancelled, state cancelled, reason session_cancelled", result)
	}
	readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitPromptSubject) // not retracted
}

// codex ebo PR2 consult: prompt A is cancelled, prompt B starts, and the
// peer's late answer to A lands on the same thread after B. It is newer
// than B, but it refs A, so it must never complete B.
func TestLateAnswerToCancelledPromptNeverCompletesTheNext(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = 5 * time.Second
	live, sessionID, threadID := newLiveSession(t, cfg, "late-answer")
	live.send(promptRequest(3, sessionID, "prompt A"))
	live.readUntilUpdate("agent_thought_chunk")
	promptA := readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitPromptSubject).Header.ID
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, sessionID))
	if stop := live.readUntilResult()["result"].(map[string]any)["stopReason"]; stop != StopReasonCancelled {
		t.Fatalf("prompt A stopReason = %v, want cancelled", stop)
	}

	live.send(promptRequest(4, sessionID, "prompt B"))
	live.readUntilUpdate("agent_thought_chunk")
	promptB, ok := newestPromptOnThread(cfg.Root, cfg.To, threadID)
	if !ok || promptB == promptA {
		t.Fatalf("setup: prompt B not delivered (got %q, A %q)", promptB, promptA)
	}
	deliverReplyWithRefs(t, cfg, threadID, "late answer to A", promptA)
	time.Sleep(3 * cfg.PollInterval) // give the wait loop a chance to (wrongly) take A's answer
	deliverReplyWithRefs(t, cfg, threadID, "answer to B", promptB)
	result := live.readUntilResult()["result"].(map[string]any)
	reply := result["_meta"].(map[string]any)["amq"].(map[string]any)["reply"]
	if result["stopReason"] != StopReasonEndTurn || reply != "answer to B" {
		t.Fatalf("prompt B = %v, want end_turn with B's own answer, never A's late one", result)
	}
}
