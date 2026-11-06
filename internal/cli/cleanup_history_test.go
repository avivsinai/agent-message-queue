package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
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

// History cleanup keeps a read review request nobody answered, so thread open
// still lists it (a 3-day-old open review request removed by
// --history-max-size 1B, review of bead agent-message-queue-9df).
func TestCleanupHistoryKeepsOpenObligations(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{{name: "rr", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest}}, func(int, threadOpenMessage) time.Time {
		return time.Now().UTC().Add(-3 * 24 * time.Hour)
	})
	s1 := filepath.Join(base, "s1")
	if err := os.Rename(filepath.Join(fsq.AgentInboxNew(s1, "beta"), ids["rr"]+".md"), filepath.Join(fsq.AgentInboxCur(s1, "beta"), ids["rr"]+".md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "1B", "--root", s1, "--yes"})
	}); err != nil {
		t.Fatal(err)
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 1 {
		t.Fatalf("open obligations after cleanup = %d, want 1", len(after.Obligations))
	}
}
