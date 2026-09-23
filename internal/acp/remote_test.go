package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestRemotePromptSubmitsToTargetAndReturnsResult is the happy path for
// remote mode (bead agent-message-queue-611.28): an ACP prompt is submitted
// to the amq-remote target through the endpoint socket, and the native result
// comes back as the agent message. Socket paths must stay short, so the root
// lives directly under the system temp root.
func TestRemotePromptSubmitsToTargetAndReturnsResult(t *testing.T) {
	root, err := os.MkdirTemp("", "acpr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, remoteStateDir)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	cfg := Config{
		Root:              root,
		RemoteTarget:      "fake",
		RemoteNative:      "fake",
		StateDir:          filepath.Join(root, "meta", "acp"),
		TurnTimeout:       2 * time.Second,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 25 * time.Millisecond,
	}
	live, sessionID, _ := newLiveSession(t, cfg, "dm-1")
	eventID := strings.Repeat("ab", 32)
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"say hi"}],"_meta":{"nostr":{"eventId":%q}}}}`, sessionID, eventID))

	requestID, err := remoteRequestID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if rt.Complete(requestID, "hi from the native session") {
				return
			}
		}
	}()

	update := live.readUntilUpdate("agent_message_chunk")
	text := update["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any)["text"]
	if text != "hi from the native session" {
		t.Fatalf("agent message = %v, want the native result", text)
	}
	result := live.readUntilResult()["result"].(map[string]any)
	if result["stopReason"] != StopReasonEndTurn {
		t.Fatalf("stopReason = %v, want end_turn: %v", result["stopReason"], result)
	}
	remote := result["_meta"].(map[string]any)["remote"].(map[string]any)
	if remote["state"] != "completed" || remote["target"] != "fake" {
		t.Fatalf("remote meta = %v", remote)
	}
}

// remoteServer runs a real endpoint socket with att and returns an ACP server
// in remote mode pinned to target "fake" and native session "fake".
func remoteServer(t *testing.T, att core.Attachment, crash core.CrashPoint) *Server {
	t.Helper()
	root, err := os.MkdirTemp("", "rv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, remoteStateDir)
	store, err := requests.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Crash: crash})
	ep.Register(att)
	srv, err := ipc.Listen(dir, ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })
	return NewServer(Config{Root: root, RemoteTarget: "fake", RemoteNative: "fake", HeartbeatInterval: 10 * time.Millisecond, TurnTimeout: time.Second}, "test")
}

func newTurn() *turnState {
	return &turnState{done: make(chan struct{}), ready: make(chan struct{})}
}

type noCancel struct{ *fake.Runtime }

func (r noCancel) CancelExact(requests.Key, string) (core.CancelEvidence, error) {
	return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "native cancel unavailable"}, nil
}

// Codex #876 P1 #1: an unsupported cancel was reported as a plain cancelled
// turn while the native work kept running.
func TestRemoteUnsupportedCancelSaysWorkContinues(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, noCancel{rt}, nil)
	turn := newTurn()
	var said []string
	result, rpcErr := s.runRemote("s", "hello", strings.Repeat("a", 64), turn, func(v any) error {
		note := v.(sessionUpdateNotification)
		if note.Params.Update.SessionUpdate == "agent_message_chunk" {
			said = append(said, note.Params.Update.Content.Text)
		}
		s.mu.Lock()
		if turn.settleLocked("session_cancelled") {
			close(turn.done)
		}
		s.mu.Unlock()
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	got := result.(remotePromptResult)
	if got.Meta.Remote.Cancel != string(protocol.CancelUnsupported) || len(said) != 1 || !strings.Contains(said[0], "work continues") {
		t.Fatalf("result=%+v said=%q", got, said)
	}
}

// Codex #876 P1 #2: an error after the native run was reported as
// not_submitted with no request reference.
func TestRemoteErrorAfterNativeRunKeepsRequest(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, func(point string) error {
		if point == core.PointAfterNative {
			return core.ErrCrashed
		}
		return nil
	})
	result, rpcErr := s.runRemote("s", "hello", strings.Repeat("b", 64), newTurn(), func(any) error { return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	got := result.(remotePromptResult).Meta.Remote
	if got.State == remoteNotSubmitted || got.RequestRef == "" {
		t.Fatalf("native run exists but result is %+v", got)
	}
}

type movingNative struct {
	*fake.Runtime
	id string
}

func (m *movingNative) NativeSessionID() string { return m.id }

// Codex #876 P1 #3: a replacement native session behind the same target
// received the owner's prompts.
func TestRemoteRefusesReplacementNativeSession(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, &movingNative{Runtime: rt, id: "replacement"}, nil)
	result, rpcErr := s.runRemote("s", "hello", "", newTurn(), func(any) error { return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	got := result.(remotePromptResult)
	if got.Meta.Remote.Code != string(protocol.CodeUnshared) || len(rt.Snapshot().RunningKeys) != 0 {
		t.Fatalf("replacement session was not refused: %+v", got)
	}
}

// Codex #876 P2 #4: a redelivered event after the endpoint reattached under a
// new epoch got request_conflict instead of its stored result.
func TestRemoteRedeliveryAfterReattachReturnsStoredResult(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	eventID := strings.Repeat("c", 64)
	id, _ := remoteRequestID(eventID)
	run := func(emit func(any) error) remotePromptResult {
		t.Helper()
		result, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), emit)
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		return result.(remotePromptResult)
	}
	if first := run(func(any) error { rt.Complete(id, "native result"); return nil }); first.StopReason != StopReasonEndTurn {
		t.Fatalf("first = %+v", first)
	}
	rt.SwitchSession("e_2")
	if second := run(func(any) error { return nil }); second.StopReason != StopReasonEndTurn || second.Meta.Remote.State != "completed" {
		t.Fatalf("redelivery = %+v", second)
	}
}

// Codex #876 P2 #5: a cancel that settled first was overwritten by a
// completed snapshot.
func TestRemoteSettledCancelWins(t *testing.T) {
	r := &remoteTurn{s: NewServer(Config{}, "test"), emit: func(any) error { return nil }, turn: &turnState{done: make(chan struct{}), outcome: "session_cancelled"}}
	close(r.turn.done)
	result, rpcErr := r.follow(protocol.Snapshot{State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonCancelled {
		t.Fatalf("settled cancel overwritten: %+v", got)
	}
}
