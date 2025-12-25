package thread

import (
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
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	msg1 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-1",
			From:    "codex",
			To:      []string{"cloudcode"},
			Thread:  "p2p/codex__cloudcode",
			Subject: "Hello",
			Created: now.Format(time.RFC3339Nano),
		},
		Body: "First",
	}
	msg2 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-2",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/codex__cloudcode",
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

	if _, err := fsq.DeliverToInbox(root, "cloudcode", "msg-1.md", data1); err != nil {
		t.Fatalf("deliver msg1: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-2.md", data2); err != nil {
		t.Fatalf("deliver msg2: %v", err)
	}

	entries, err := Collect(root, "p2p/codex__cloudcode", []string{"codex", "cloudcode"}, false, nil)
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
