//go:build darwin || linux

package fsq

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadDLQEnvelopeRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, _, err := ReadDLQEnvelopePath(path)
	if err == nil {
		t.Fatal("expected FIFO DLQ envelope to be rejected")
	}
}

func TestMoveCurToDLQRejectsEscapingTmpSymlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	filename := "escape_tmp.md"
	source := filepath.Join(AgentInboxCur(root, "alice"), filename)
	if err := os.WriteFile(source, []byte("contained"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	outside := t.TempDir()
	tmpDir := AgentDLQTmp(root, "alice")
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatalf("remove dlq tmp: %v", err)
	}
	if err := os.Symlink(outside, tmpDir); err != nil {
		t.Fatalf("symlink dlq tmp: %v", err)
	}

	if _, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "escape_tmp", "parse_error", "test"); err == nil {
		t.Fatal("MoveCurToDLQ succeeded through an escaping dlq/tmp symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("escaping dlq/tmp received %d files", len(entries))
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source should remain after refused DLQ delivery: %v", err)
	}
}

func TestRetryFromDLQRejectsEscapingCurSymlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dlqPath := createDLQMessage(t, root, "alice", "escape_retry.md", []byte("contained"))

	outside := t.TempDir()
	curDir := AgentDLQCur(root, "alice")
	if err := os.RemoveAll(curDir); err != nil {
		t.Fatalf("remove dlq cur: %v", err)
	}
	if err := os.Symlink(outside, curDir); err != nil {
		t.Fatalf("symlink dlq cur: %v", err)
	}

	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false); err == nil {
		t.Fatal("RetryFromDLQ succeeded through an escaping dlq/cur symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("escaping dlq/cur received %d files", len(entries))
	}
	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "alice"), "escape_retry.md")); !os.IsNotExist(err) {
		t.Fatalf("retry redelivered before the confined envelope update, stat err: %v", err)
	}
}
