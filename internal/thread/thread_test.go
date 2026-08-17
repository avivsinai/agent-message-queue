package thread

import (
	"errors"
	"fmt"
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

	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	if _, err := fsq.DeliverToInbox(deliveryRoot, "claude", "msg-1.md", data1); err != nil {
		t.Fatalf("deliver msg1: %v", err)
	}
	if _, err := fsq.DeliverToInbox(deliveryRoot, "codex", "msg-2.md", data2); err != nil {
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

func TestCollectIgnoresDotfilesAndNonMarkdown(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	visible := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "visible",
			From:    "codex",
			To:      []string{"claude"},
			Thread:  "p2p/claude__codex",
			Created: time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: "visible\n",
	}
	hidden := visible
	hidden.Header.ID = "hidden"
	data, err := visible.Marshal()
	if err != nil {
		t.Fatalf("marshal visible: %v", err)
	}
	hiddenData, err := hidden.Marshal()
	if err != nil {
		t.Fatalf("marshal hidden: %v", err)
	}
	inbox := fsq.AgentInboxNew(root, "codex")
	if err := os.WriteFile(filepath.Join(inbox, "visible.md"), data, 0o600); err != nil {
		t.Fatalf("write visible.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, ".hidden.md"), hiddenData, 0o600); err != nil {
		t.Fatalf("write .hidden.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "notes.txt"), []byte("not a message"), 0o600); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	entries, err := Collect(root, "p2p/claude__codex", []string{"codex"}, false, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "visible" {
		t.Fatalf("entries = %#v, want only visible.md", entries)
	}
}

func TestCollectOnErrorNilStillScansLaterMessages(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	good := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "good",
			From:    "codex",
			To:      []string{"claude"},
			Thread:  "p2p/claude__codex",
			Created: time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: "good\n",
	}
	data, err := good.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	inbox := fsq.AgentInboxNew(root, "codex")
	if err := os.WriteFile(filepath.Join(inbox, "a-bad.md"), []byte("not a message"), 0o600); err != nil {
		t.Fatalf("write a-bad.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "b-good.md"), data, 0o600); err != nil {
		t.Fatalf("write b-good.md: %v", err)
	}

	entries, err := Collect(root, "p2p/claude__codex", []string{"codex"}, false, func(string, error) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "good" {
		t.Fatalf("entries = %#v, want b-good.md after skipped corrupt file", entries)
	}
}

func TestCollectOnErrorPropagatesCallbackError(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	path := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad.md")
	if err := os.WriteFile(path, []byte("not a message"), 0o600); err != nil {
		t.Fatalf("write bad message: %v", err)
	}

	want := errors.New("stop scan")
	_, err := Collect(root, "p2p/claude__codex", []string{"codex"}, false, func(string, error) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Collect error = %v, want callback error", err)
	}
}

func TestCollectScansNewCurAndOutboxDedupesAndReportsCallbackPath(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	write := func(dir, name, id, body string, created time.Time) {
		t.Helper()
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      id,
				From:    "codex",
				To:      []string{"claude"},
				Thread:  "p2p/claude__codex",
				Created: created.Format(time.RFC3339Nano),
			},
			Body: body,
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	inboxNew := fsq.AgentInboxNew(root, "codex")
	inboxCur := fsq.AgentInboxCur(root, "codex")
	outbox := fsq.AgentOutboxSent(root, "codex")
	write(inboxNew, "from-new.md", "from-new", "new\n", now)
	write(inboxCur, "from-cur.md", "from-cur", "cur\n", now.Add(time.Second))
	write(outbox, "from-sent.md", "from-sent", "sent\n", now.Add(2*time.Second))
	write(outbox, "dup-of-new.md", "from-new", "dup\n", now.Add(3*time.Second))

	corruptPath := filepath.Join(inboxCur, "zz-corrupt.md")
	if err := os.WriteFile(corruptPath, []byte("not a message"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	for _, includeBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("includeBody=%v", includeBody), func(t *testing.T) {
			var gotPath string
			var gotErr error
			entries, err := Collect(root, "p2p/claude__codex", []string{"codex"}, includeBody, func(path string, err error) error {
				gotPath = path
				gotErr = err
				return nil
			})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if gotPath != corruptPath {
				t.Fatalf("onError path = %q, want %q", gotPath, corruptPath)
			}
			if gotErr == nil {
				t.Fatal("onError err is nil, want parse error")
			}
			gotIDs := make([]string, len(entries))
			for i, e := range entries {
				gotIDs[i] = e.ID
				if includeBody && e.Body == "" {
					t.Fatalf("includeBody entry %s has empty body", e.ID)
				}
				if !includeBody && e.Body != "" {
					t.Fatalf("header-only entry %s has body %q", e.ID, e.Body)
				}
			}
			want := []string{"from-new", "from-cur", "from-sent"}
			if len(gotIDs) != 3 || gotIDs[0] != want[0] || gotIDs[1] != want[1] || gotIDs[2] != want[2] {
				t.Fatalf("ids = %v, want %v (duplicate from-new skipped)", gotIDs, want)
			}
		})
	}
}
