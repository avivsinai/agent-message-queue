package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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

// Codex #876 r2 P1 #2: a redelivery whose lookup failed asserted
// not_submitted while the original native run existed.
func TestRemoteRedeliveryLookupFailureIsUncertain(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	eventID := strings.Repeat("e", 64)
	if _, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	path := ipc.SocketPath(filepath.Join(s.cfg.Root, remoteStateDir))
	if err := os.Rename(path, path+".hold"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(path+".hold", path) })
	result, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult).Meta.Remote; got.State != remoteUncertain || got.RequestRef == "" {
		t.Fatalf("redelivery = %+v", got)
	}
}

// Codex #876 r2 P1 #3: the replay path read a completed request with a wrong
// native pin.
func TestRemoteReplayWithWrongPinIsRefused(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	eventID := strings.Repeat("f", 64)
	id, _ := remoteRequestID(eventID)
	if _, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { rt.Complete(id, "private result"); return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	s.cfg.RemoteNative = "another-session"
	result, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason == StopReasonEndTurn || got.Meta.Remote.Code != string(protocol.CodeUnshared) {
		t.Fatalf("wrong pin read the request: %+v", got)
	}
}

// Codex #876 r2 P2 #4: a busy-rejected event could not be retried after the
// target became free.
func TestRemoteBusyRedeliveryRetries(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	busyID := "d2c80e1d-feb7-4c10-959e-23456789abcd"
	resp, err := ipc.Call(filepath.Join(s.cfg.Root, remoteStateDir), ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: busyID, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "occupy", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("d", 64)
	id, _ := remoteRequestID(eventID)
	s.cfg.TurnTimeout = 40 * time.Millisecond
	s.cfg.PollInterval = 5 * time.Millisecond
	first, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { return nil })
	if rpcErr != nil || first.(remotePromptResult).Meta.Remote.Code != string(protocol.CodeBusy) {
		t.Fatalf("first = %+v %v", first, rpcErr)
	}
	if !rt.Complete(busyID, "finished") {
		t.Fatal("no busy run")
	}
	second, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(any) error { rt.Complete(id, "recovered"); return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := second.(remotePromptResult); got.StopReason != StopReasonEndTurn {
		t.Fatalf("busy redelivery = %+v", got)
	}
}

// TestRemoteBusyDMWaitsThenRuns is agent-message-queue-611.34: a DM that
// arrives while the session is busy is queued once, then runs on the same
// request id after the session is free.
func TestRemoteBusyDMWaitsThenRuns(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	s.cfg.PollInterval = 5 * time.Millisecond
	s.cfg.TurnTimeout = 2 * time.Second
	occupy := "d2c80e1d-feb7-4c10-959e-23456789abce"
	resp, err := ipc.Call(filepath.Join(s.cfg.Root, remoteStateDir), ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: occupy, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "local turn", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("e", 64)
	id, err := remoteRequestID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var thoughts []string
	done := make(chan remotePromptResult, 1)
	go func() {
		result, rpcErr := s.runRemote("s", "hello from buzz", eventID, newTurn(), func(v any) error {
			note := v.(sessionUpdateNotification)
			if note.Params.Update.SessionUpdate == "agent_thought_chunk" {
				mu.Lock()
				thoughts = append(thoughts, note.Params.Update.Content.Text)
				mu.Unlock()
			}
			return nil
		})
		if rpcErr != nil {
			t.Errorf("run: %v", rpcErr)
		}
		done <- result.(remotePromptResult)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && queuedThoughts(&mu, thoughts) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	queued := queuedThoughts(&mu, thoughts)
	if queued != 1 {
		t.Fatalf("queued thoughts = %q", thoughts)
	}
	if rt.HasRun(id) {
		t.Fatal("dm ran while the session was busy")
	}
	if !rt.Complete(occupy, "local done") {
		t.Fatal("occupy did not complete")
	}
	for time.Now().Before(deadline.Add(time.Second)) {
		if rt.Complete(id, "ran after the wait") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := <-done
	if got.StopReason != StopReasonEndTurn {
		t.Fatalf("result = %+v", got)
	}
	if queuedThoughts(&mu, thoughts) != 1 {
		mu.Lock()
		gotThoughts := append([]string(nil), thoughts...)
		mu.Unlock()
		t.Fatalf("queued thoughts after admit = %q", gotThoughts)
	}
}

func queuedThoughts(mu *sync.Mutex, thoughts []string) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, text := range thoughts {
		if strings.HasPrefix(text, "Queued:") {
			n++
		}
	}
	return n
}

// TestRemoteCancelWhileQueuedSubmitsNothing is agent-message-queue-611.34:
// session/cancel during the busy wait stops retrying and never admits the DM.
func TestRemoteCancelWhileQueuedSubmitsNothing(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	s.cfg.PollInterval = 5 * time.Millisecond
	s.cfg.TurnTimeout = 2 * time.Second
	occupy := "d2c80e1d-feb7-4c10-959e-23456789abcf"
	resp, err := ipc.Call(filepath.Join(s.cfg.Root, remoteStateDir), ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: occupy, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "local turn", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("9", 64)
	id, err := remoteRequestID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	turn := newTurn()
	sawQueue := make(chan struct{}, 1)
	done := make(chan remotePromptResult, 1)
	go func() {
		result, rpcErr := s.runRemote("s", "hello from buzz", eventID, turn, func(v any) error {
			note := v.(sessionUpdateNotification)
			if note.Params.Update.SessionUpdate == "agent_thought_chunk" && strings.HasPrefix(note.Params.Update.Content.Text, "Queued:") {
				select {
				case sawQueue <- struct{}{}:
				default:
				}
			}
			return nil
		})
		if rpcErr != nil {
			t.Errorf("run: %v", rpcErr)
		}
		done <- result.(remotePromptResult)
	}()
	select {
	case <-sawQueue:
	case <-time.After(time.Second):
		t.Fatal("dm was not queued")
	}
	s.mu.Lock()
	if turn.settleLocked("session_cancelled") {
		close(turn.done)
	}
	s.mu.Unlock()
	got := <-done
	if got.StopReason != StopReasonCancelled || rt.HasRun(id) {
		t.Fatalf("cancel admitted the dm: %+v hasRun=%v", got, rt.HasRun(id))
	}
}

// Bead agent-message-queue-611.31: in binding mode a prompt answers "Not
// connected" until a session is bound, then runs in the bound session.
func TestBindingModeFollowsTheBoundSession(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	rt := fake.New("fake", "e_1")
	bound := remoteServer(t, rt, nil)
	s := NewServer(Config{RemoteBinding: true, StateDir: t.TempDir(), HeartbeatInterval: 10 * time.Millisecond, TurnTimeout: time.Second}, "test")

	var said []string
	emit := func(v any) error {
		if note := v.(sessionUpdateNotification); note.Params.Update.SessionUpdate == "agent_message_chunk" {
			said = append(said, note.Params.Update.Content.Text)
		}
		return nil
	}
	result, rpcErr := s.runRemote("s", "hello", "", newTurn(), emit)
	if rpcErr != nil || len(said) != 1 || !strings.HasPrefix(said[0], "Not connected") {
		t.Fatalf("unbound: result=%+v said=%q err=%v", result, said, rpcErr)
	}

	if err := binding.Write(binding.Binding{Root: bound.cfg.Root, Target: "fake", NativeSession: "fake"}); err != nil {
		t.Fatal(err)
	}
	eventID := strings.Repeat("9", 64)
	id, _ := remoteRequestID(eventID)
	result, rpcErr = s.runRemote("s", "hello", eventID, newTurn(), func(any) error { rt.Complete(id, "from the bound session"); return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonEndTurn || got.Meta.Remote.Target != "fake" {
		t.Fatalf("bound: %+v", got)
	}
}

// Codex #885 P1 #1: a redelivered event followed the current binding, so a
// rebind between deliveries ran it again in the new session.
func TestRedeliveryAfterRebindStaysOnTheFirstSession(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	a, b := fake.New("fake", "e_1"), fake.New("fake", "e_1")
	serverA, serverB := remoteServer(t, a, nil), remoteServer(t, b, nil)
	s := NewServer(Config{RemoteBinding: true, StateDir: t.TempDir(), TurnTimeout: time.Second, HeartbeatInterval: 10 * time.Millisecond}, "test")
	eventID := strings.Repeat("a", 64)
	id, _ := remoteRequestID(eventID)
	run := func(rt *fake.Runtime, label string) string {
		t.Helper()
		out := ""
		if _, rpcErr := s.runRemote("s", "hello", eventID, newTurn(), func(v any) error {
			note := v.(sessionUpdateNotification)
			if note.Params.Update.SessionUpdate == "agent_thought_chunk" {
				rt.Complete(id, "result from "+label)
			}
			if note.Params.Update.SessionUpdate == "agent_message_chunk" {
				out += note.Params.Update.Content.Text
			}
			return nil
		}); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		return out
	}
	if err := binding.Write(binding.Binding{Root: serverA.cfg.Root, Target: "fake", NativeSession: "fake"}); err != nil {
		t.Fatal(err)
	}
	first := run(a, "A")
	if err := binding.Write(binding.Binding{Root: serverB.cfg.Root, Target: "fake", NativeSession: "fake"}); err != nil {
		t.Fatal(err)
	}
	if second := run(b, "B"); first != "result from A" || second != first {
		t.Fatalf("first=%q second=%q; the redelivery must return the first session's result", first, second)
	}
}

// canonicalTempDir is t.TempDir with symlinks resolved: the binding override
// refuses a symlinked path, and macOS temp dirs live under the /var symlink.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Codex #885 r2 P1: a concurrent first delivery that lost the event claim
// used its own captured binding. The loser must follow the winner.
func TestEventClaimLoserFollowsTheWinner(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	s := NewServer(Config{RemoteBinding: true, StateDir: canonicalTempDir(t)}, "test")
	eventID := strings.Repeat("b", 64)
	winner := binding.Binding{Root: "/winner", Target: "claude:1", NativeSession: "s-winner"}
	raw, _ := json.Marshal(winner)
	if won, err := createExclusive(filepath.Join(s.cfg.StateDir, "remote-events", eventID+".json"), raw); err != nil || !won {
		t.Fatalf("seed claim: won=%v err=%v", won, err)
	}
	if err := binding.Write(binding.Binding{Root: "/loser", Target: "claude:2", NativeSession: "s-loser"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.turnBinding(eventID)
	if err != nil || !got.Same(winner) {
		t.Fatalf("turn binding = %+v %v; want the winner's", got, err)
	}
}

// Codex #885 r3 P1: a reader used a visible claim before its directory entry
// was durable. Every returned claim is synced first.
func TestExistingEventClaimIsMadeDurableBeforeUse(t *testing.T) {
	s := NewServer(Config{RemoteBinding: true, StateDir: canonicalTempDir(t)}, "test")
	eventID := strings.Repeat("c", 64)
	claimDir := filepath.Join(s.cfg.StateDir, "remote-events")
	raw, _ := json.Marshal(binding.Binding{Root: "/r", Target: "claude:1", NativeSession: "s"})
	if won, err := createExclusive(filepath.Join(claimDir, eventID+".json"), raw); err != nil || !won {
		t.Fatalf("seed claim: %v %v", won, err)
	}
	synced := false
	restore := fsq.SyncDirAmbientSwapForTest(func(dir string) error {
		if dir == claimDir {
			synced = true
		}
		return nil
	})
	defer restore()
	if _, err := s.turnBinding(eventID); err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("an existing claim was returned before its directory was synced")
	}
}
