package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// cleanup with a size policy brings each mailbox's history under the size it
// is given, oldest first, and leaves unread mail (unbounded mailboxes: 13.4k
// read, 14.7k sent and 13.5k receipt files in one mailbox, bead
// agent-message-queue-9df).
func TestCleanupHistoryMaxSizeRemovesOldestHistory(t *testing.T) {
	rootPath := newCleanupMailboxRoot(t, "claude")
	base := fsq.AgentBase(rootPath, "claude")
	files := map[string]string{
		"inbox/cur/2026-01-01T00-00-00.000Z_pid1_a.md":                   "old read",
		"outbox/sent/2026-01-02T00-00-00.000Z_pid1_b.md":                 "old sent",
		"receipts/2026-01-03T00-00-00.000Z_pid1_c__claude__drained.json": "receipt1",
		"inbox/cur/2026-02-01T00-00-00.000Z_pid1_d.md":                   "new read",
		"inbox/new/2025-01-01T00-00-00.000Z_pid1_e.md":                   "unread!!",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(base, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "10B", "--root", rootPath, "--yes"})
	}); err != nil {
		t.Fatalf("cleanup --history-max-size: %v", err)
	}
	for rel := range files {
		_, err := os.Lstat(filepath.Join(base, rel))
		kept := strings.Contains(rel, "2026-02-01") || strings.HasPrefix(rel, "inbox/new")
		if kept != (err == nil) {
			t.Fatalf("%s kept=%v, want %v", rel, err == nil, kept)
		}
	}
}
