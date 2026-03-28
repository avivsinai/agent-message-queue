package kanban

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/coder/websocket"
)

func TestRunBridgeDeliversWebsocketEvents(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runtime/ws" {
			http.NotFound(w, r)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}()

		ctx := r.Context()
		writeWS(t, ctx, conn, `{"type":"snapshot","currentProjectId":"workspace-1","workspaceState":{"repoPath":"/repo/path","board":{"columns":[{"id":"in_progress","title":"In Progress","cards":[{"id":"task-1","prompt":"Refactor bridge"}]}]},"sessions":{}}}`)
		writeWS(t, ctx, conn, `{"type":"task_sessions_updated","workspaceId":"workspace-1","summaries":[{"taskId":"task-1","state":"running","agentId":"codex","workspacePath":"/repo/path","updatedAt":1}]}`)
		writeWS(t, ctx, conn, `{"type":"task_ready_for_review","workspaceId":"workspace-1","taskId":"task-1","triggeredAt":2}`)

		<-release
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunBridge(ctx, BridgeConfig{
			AgentHandle:    "codex",
			AMQRoot:        root,
			URL:            strings.Replace(server.URL, "http://", "ws://", 1) + "/api/runtime/ws",
			ReconnectDelay: 25 * time.Millisecond,
		})
	}()

	paths := waitForInboxMessages(t, fsq.AgentInboxNew(root, "codex"), 2)
	close(release)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("RunBridge error = %v, want %v", err, context.Canceled)
	}

	var subjects []string
	for _, path := range paths {
		msg, readErr := format.ReadMessageFile(path)
		if readErr != nil {
			t.Fatalf("ReadMessageFile(%s): %v", path, readErr)
		}
		subjects = append(subjects, msg.Header.Subject)
	}

	joined := strings.Join(subjects, "\n")
	if !strings.Contains(joined, "[kanban] running: Refactor bridge") {
		t.Fatalf("subjects missing running notification:\n%s", joined)
	}
	if !strings.Contains(joined, "[kanban] review: Refactor bridge") {
		t.Fatalf("subjects missing review notification:\n%s", joined)
	}
}

func writeWS(t *testing.T, ctx context.Context, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("conn.Write(%s): %v", payload, err)
	}
}

func waitForInboxMessages(t *testing.T, dir string, want int) []string {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			t.Fatalf("Glob(%s): %v", dir, err)
		}
		if len(entries) >= want {
			return entries
		}
		time.Sleep(20 * time.Millisecond)
	}

	entries, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	t.Fatalf("timed out waiting for %d inbox messages in %s (got %d)", want, dir, len(entries))
	return nil
}

func TestProcessBridgeMessageIgnoresUnknownTypes(t *testing.T) {
	state := newBridgeState()
	notifications, err := processBridgeMessage([]byte(`{"type":"projects_updated"}`), state)
	if err != nil {
		t.Fatalf("processBridgeMessage error = %v", err)
	}
	if len(notifications) != 0 {
		t.Fatalf("len(notifications) = %d, want 0", len(notifications))
	}
}

func TestValidateBridgeConfig(t *testing.T) {
	err := validateBridgeConfig(BridgeConfig{
		AgentHandle:    "codex",
		AMQRoot:        "/tmp/root",
		URL:            "http://127.0.0.1:3484/api/runtime/ws",
		ReconnectDelay: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid websocket URL scheme") {
		t.Fatalf("validateBridgeConfig error = %v", err)
	}

	err = validateBridgeConfig(BridgeConfig{
		AgentHandle:    "codex",
		AMQRoot:        "/tmp/root",
		URL:            "ws://127.0.0.1:3484/api/runtime/ws",
		ReconnectDelay: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "reconnect delay") {
		t.Fatalf("validateBridgeConfig reconnect error = %v", err)
	}

	err = validateBridgeConfig(BridgeConfig{
		AgentHandle:    "codex",
		AMQRoot:        "/tmp/root",
		URL:            "ws://127.0.0.1:3484/api/runtime/ws",
		ReconnectDelay: time.Second,
	})
	if err != nil {
		t.Fatalf("validateBridgeConfig valid config error = %v", err)
	}
}

func Example_processBridgeMessage() {
	state := newBridgeState()
	notifications, _ := processBridgeMessage([]byte(`{"type":"task_ready_for_review","workspaceId":"workspace-1","taskId":"task-1","triggeredAt":2}`), state)
	fmt.Println(len(notifications))
	// Output: 1
}
