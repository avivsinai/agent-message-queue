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
