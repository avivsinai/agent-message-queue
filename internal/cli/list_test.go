package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestListCanInspectFlagShapedLegacyMailboxButDrainRejectsIt(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	legacy := "-legacy"
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		if err := os.MkdirAll(fsq.AgentMailboxPath(root, legacy, leaf), 0o700); err != nil {
			t.Fatalf("create legacy mailbox leaf %s: %v", leaf, err)
		}
	}
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "legacy-message",
			From:    "bob",
			To:      []string{legacy},
			Thread:  "p2p/-legacy__bob",
			Subject: "recover me",
			Created: "2026-07-29T07:00:00Z",
		},
		Body: "legacy body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, legacy), "legacy-message.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", root, "--me=-legacy", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("read-only legacy list: %v", err)
	}
	var items []listItem
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("decode legacy list: %v\n%s", err, stdout)
	}
	if len(items) != 1 || items[0].ID != "legacy-message" {
		t.Fatalf("legacy list items = %#v", items)
	}

	if err := runDrain([]string{"--root", root, "--me=-legacy"}); err == nil {
		t.Fatal("drain accepted flag-shaped legacy handle")
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, legacy), "legacy-message.md")); err != nil {
		t.Fatalf("rejected drain changed legacy message: %v", err)
	}
}

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
		if _, err := deliverToInboxForTest(t, root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
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
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		args = append(args, "--offset", strconv.Itoa(offset))
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

func TestListSessionDoesNotExistReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, root)
	t.Setenv(envSession, "")
	err := runList([]string{"--me", "alice", "--session", "missing", "--new"})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("exit = %d, want %d (err=%v)", GetExitCode(err), ExitNotFound, err)
	}
}
