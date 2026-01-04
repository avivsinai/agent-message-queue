package thread

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCollectThread(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs claude: %v", err)
	}

	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	msg1 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-1",
			From:    "codex",
			To:      []string{"claude"},
			Thread:  "p2p/claude__codex",
			Subject: "Hello",
			Created: now.Format(time.RFC3339Nano),
		},
		Body: "First",
	}
	msg2 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-2",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "Re: Hello",
			Created: now.Add(2 * time.Second).Format(time.RFC3339Nano),
		},
		Body: "Second",
	}

	data1, err := msg1.Marshal()
	if err != nil {
		t.Fatalf("marshal msg1: %v", err)
	}
	data2, err := msg2.Marshal()
	if err != nil {
		t.Fatalf("marshal msg2: %v", err)
	}

	if _, err := fsq.DeliverToInbox(root, "claude", "msg-1.md", data1); err != nil {
		t.Fatalf("deliver msg1: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-2.md", data2); err != nil {
		t.Fatalf("deliver msg2: %v", err)
	}

	entries, err := Collect(root, "p2p/claude__codex", []string{"codex", "claude"}, false, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "msg-1" || entries[1].ID != "msg-2" {
		t.Fatalf("unexpected order: %v, %v", entries[0].ID, entries[1].ID)
	}
}

func TestCollectThreadCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}

	path := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad.md")
	if err := os.WriteFile(path, []byte("not a message"), 0o644); err != nil {
		t.Fatalf("write bad message: %v", err)
	}

	if _, err := Collect(root, "p2p/claude__codex", []string{"codex"}, false, nil); err == nil {
		t.Fatalf("expected error for corrupt message")
	}

	called := false
	entries, err := Collect(root, "p2p/claude__codex", []string{"codex"}, false, func(path string, err error) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Collect with onError: %v", err)
	}
	if !called {
		t.Fatalf("expected onError to be called")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}
