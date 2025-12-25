package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunListPagination(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages with distinct timestamps
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal msg %d: %v", i, err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	t.Run("no pagination returns all", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 0)
		if len(items) != 5 {
			t.Errorf("expected 5 items, got %d", len(items))
		}
	})

	t.Run("limit 2 returns first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 0)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-a" {
			t.Errorf("expected first item msg-a, got %s", items[0].ID)
		}
		if items[1].ID != "msg-b" {
			t.Errorf("expected second item msg-b, got %s", items[1].ID)
		}
	})

	t.Run("offset 2 skips first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 2)
		if len(items) != 3 {
			t.Errorf("expected 3 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
	})

	t.Run("limit 2 offset 2 returns middle 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 2)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
		if items[1].ID != "msg-d" {
			t.Errorf("expected second item msg-d, got %s", items[1].ID)
		}
	})

	t.Run("offset beyond range returns empty", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 100)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})
}

func runListJSON(t *testing.T, root, agent string, limit, offset int) []listItem {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json", "--new"}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if offset > 0 {
		args = append(args, "--offset", itoa(offset))
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runList(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runList: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var items []listItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return items
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
