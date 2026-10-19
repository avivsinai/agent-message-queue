package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func newCleanupMailboxRoot(t *testing.T, handle string) string {
	t.Helper()
	rootPath := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.EnsureAgentDirs(handle); err != nil {
		t.Fatal(err)
	}
	return rootPath
}

func TestCleanupMailboxRemovesEmptyMailboxThenReportsAbsent(t *testing.T) {
	rootPath := newCleanupMailboxRoot(t, "chat-1")
	// Read history in inbox/cur goes with the mailbox; only unread mail blocks.
	read := filepath.Join(fsq.AgentBase(rootPath, "chat-1"), "inbox", "cur", "read.md")
	if err := os.WriteFile(read, []byte("read"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--mailbox", "chat-1", "--root", rootPath, "--yes", "--json"}

	stdout, _, err := captureEnvOutput(t, func() error { return runCleanup(args) })
	if err != nil {
		t.Fatalf("cleanup --mailbox: %v", err)
	}
	var report mailboxCleanupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Removed || report.Absent {
		t.Fatalf("first report=%s err=%v", stdout, err)
	}
	if _, err := os.Lstat(fsq.AgentBase(rootPath, "chat-1")); !os.IsNotExist(err) {
		t.Fatalf("mailbox after cleanup: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(rootPath, "agents")); err != nil || len(entries) != 0 {
		t.Fatalf("agents dir after cleanup = %v, %v", entries, err)
	}

	stdout, _, err = captureEnvOutput(t, func() error { return runCleanup(args) })
	if err != nil {
		t.Fatalf("second cleanup --mailbox: %v", err)
	}
	report = mailboxCleanupReport{}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Removed || !report.Absent {
		t.Fatalf("second report=%s err=%v", stdout, err)
	}
}

func TestCleanupMailboxRefusesMailboxHoldingMail(t *testing.T) {
	rootPath := newCleanupMailboxRoot(t, "chat-1")
	message := filepath.Join(fsq.AgentInboxNew(rootPath, "chat-1"), "pending.md")
	if err := os.WriteFile(message, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--mailbox", "chat-1", "--root", rootPath, "--yes", "--json"})
	})
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("cleanup --mailbox exit=%d err=%v", GetExitCode(err), err)
	}
	var report mailboxCleanupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("report=%s err=%v", stdout, err)
	}
	if report.Removed || report.Refusal != mailboxRefusalMail || report.Inbox == nil || report.Inbox.New != 1 {
		t.Fatalf("report=%s", stdout)
	}
	if _, err := os.Lstat(message); err != nil {
		t.Fatalf("message after refusal: %v", err)
	}
}

func TestCleanupMailboxFinishesInterruptedRemoval(t *testing.T) {
	rootPath := t.TempDir()
	orphan := filepath.Join(rootPath, fsq.MailboxRemovalDir, "chat-1-0123456789abcdef01234567")
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		if err := os.MkdirAll(filepath.Join(orphan, filepath.FromSlash(string(leaf))), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--mailbox", "chat-1", "--root", rootPath, "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("cleanup --mailbox: %v", err)
	}
	var report mailboxCleanupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Absent || report.Removed ||
		len(report.Recovered) != 1 || filepath.Base(report.Recovered[0]) != filepath.Base(orphan) {
		t.Fatalf("report=%s err=%v", stdout, err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan after cleanup: %v", err)
	}
}
