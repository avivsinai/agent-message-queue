package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// TestRemotePromptSubmitsToTargetAndReturnsResult is the happy path for
// remote mode (happy path): an ACP prompt is submitted
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

	requestID, err := remoteturn.RequestID(eventID)
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
	root := endpointRoot(t)
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

// endpointRoot is a fresh AMQ root under the caller's TMPDIR, named short so
// the endpoint socket beneath it fits the Unix socket path limit (104 bytes
// on macOS). A TMPDIR too long for it fails setup and names the limit.
func endpointRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if sock := ipc.SocketPath(filepath.Join(root, remoteStateDir)); runtime.GOOS != "windows" && len(sock) >= 104 {
		t.Fatalf("endpoint socket %s is %d bytes, over the 104-byte Unix socket path limit; set a shorter TMPDIR", sock, len(sock))
	}
	return root
}

func newTurn() *turnState {
	return &turnState{done: make(chan struct{}), ready: make(chan struct{})}
}

// TestRemoteCancelWhileQueuedSubmitsNothing covers:
// session/cancel during the busy wait stops retrying and never admits the DM.
func TestRemoteCancelWhileQueuedSubmitsNothing(t *testing.T) {
	rt := fake.New("fake", "e_1")
	s := remoteServer(t, rt, nil)
	s.cfg.StateDir = t.TempDir()
	s.cfg.PollInterval = 5 * time.Millisecond
	s.cfg.TurnTimeout = 2 * time.Second
	occupy := "d2c80e1d-feb7-4c10-959e-23456789abcf"
	resp, err := ipc.Call(filepath.Join(s.cfg.Root, remoteStateDir), ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: occupy, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "local turn", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("9", 64)
	id, err := remoteturn.RequestID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	turn := newTurn()
	sawQueue := make(chan struct{}, 1)
	done := make(chan remoteturn.Result, 1)
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
		done <- result.(remoteturn.Result)
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
	// Regression: #889, a later delivery of the same event must not run the
	// work the owner cancelled.
	if !rt.Complete(occupy, "local done") {
		t.Fatal("occupy did not complete")
	}
	again, rpcErr := s.runRemote("s", "hello from buzz", eventID, newTurn(), func(any) error { return nil })
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if again.(remoteturn.Result).StopReason != StopReasonCancelled || rt.HasRun(id) {
		t.Fatalf("redelivery ran: %+v hasRun=%v", again, rt.HasRun(id))
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
