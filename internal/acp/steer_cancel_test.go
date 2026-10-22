package acp

import (
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
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

// Regression: #860 (reproduced at GOMAXPROCS=1), a cancel read
// right after its prompt found no turn, because the prompt reserved its turn
// inside its own goroutine, and the prompt then timed out instead of
// returning cancelled.
func TestCancelImmediatelyAfterPromptIsNotLost(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	cfg := testConfig(t)
	s := NewServer(cfg, "test")
	s.ready = true
	s.sessions["known"] = &sessionState{ID: "known", Thread: "cockpit/immediate"}
	replies := serve(t, s, promptRequest(3, "known", "do work"), `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"known"}}`)
	var reply struct {
		Result promptResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(replies[0]), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Result.StopReason != StopReasonCancelled {
		t.Fatalf("stopReason = %q reason = %q, want cancelled", reply.Result.StopReason, reply.Result.Meta.AMQ.Reason)
	}
}

// Regression: #860, a replayed steer event must not be reclassified from the
// session's current state; it reports duplicate and delivers nothing new.
func TestReplayedSteerReportsDuplicate(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, _ := newLiveSession(t, cfg, "steer-dup")
	req := func(id int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"_session/steering","params":{"sessionId":%q,"prompt":"note","_meta":{"nostr":{"eventId":%q}}}}`, id, sessionID, strings.Repeat("a", 64))
	}
	live.send(req(3))
	first := live.readUntilResult()["result"].(map[string]any)
	if first["outcome"] != SteeringStartedNewTurn {
		t.Fatalf("first steer outcome = %v", first["outcome"])
	}
	live.send(req(4))
	second := live.readUntilResult()["result"].(map[string]any)
	if second["outcome"] != SteeringDuplicate {
		t.Fatalf("replayed steer outcome = %v, want %s", second["outcome"], SteeringDuplicate)
	}
}

// Regression (agent-message-queue-37o.7, #1149 review): a replayed event's
// turn waits on its first delivery's recipient and thread, but steering went
// to the worker's current recipient on the new session's thread.
func TestSteeringFollowsAReplayedTurn(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = time.Second
	event := strings.Repeat("5", 64)
	claim := eventRecord{Schema: 1, EventID: event, MessageID: "2026-10-22T09-00-00.000Z_pid1_c0ffee00", To: "agent", Thread: cockpitThread("acp-sessionA"),
		Created: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}
	if err := rememberEvent(cfg, claim); err != nil {
		t.Fatal(err)
	}
	live, sessionID, _ := newLiveSession(t, cfg, "steer-replay")
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"build"}],"_meta":{"nostr":{"eventId":%q}}}}`, sessionID, event))
	live.readUntilUpdate("agent_thought_chunk")
	live.send(steeringRequest(4, sessionID, "use the smaller design"))
	if outcome := live.readUntilResult()["result"].(map[string]any)["outcome"]; outcome != SteeringInjected {
		t.Fatalf("steering outcome = %v, want %s", outcome, SteeringInjected)
	}
	steer := readInboxMessageSubject(t, cfg.Root, "agent", CockpitSteeringSubject)
	if steer.Header.Thread != claim.Thread || !slices.Contains(steer.Header.Refs, claim.MessageID) {
		t.Fatalf("steer thread %q refs %v; want the replayed turn's %q and %s", steer.Header.Thread, steer.Header.Refs, claim.Thread, claim.MessageID)
	}
	live.readUntilResult() // the turn ends unanswered
}
