package usagecache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// PR #1235 review: after the parent sync failed once, the next New found
// the directory and returned it without syncing the parent, so the
// directory entry was never made durable.
func TestNewSyncsParentAgainAfterAFailedSync(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "usage")
	fail := true
	synced := 0
	restore := fsq.SyncDirAmbientSwapForTest(func(d string) error {
		if d != parent {
			return nil
		}
		if fail {
			return errors.New("sync failed")
		}
		synced++
		return nil
	})
	defer restore()
	if _, err := New(dir); err == nil {
		t.Fatal("New succeeded although the parent sync failed")
	}
	fail = false
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	if synced != 1 {
		t.Fatalf("parent synced %d times after the failure, want 1", synced)
	}
}

// Takeover review of U1: pruning errors were discarded after a corrupt
// journal was safely set aside. Keep the usable recovery and report the
// failed cleanup separately.
func TestSetAsideJournalReportsCleanupFailureAfterRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{journalFile,
		journalFile + ".unreadable-20260101T000000.000000000Z",
		journalFile + ".unreadable-20260102T000000.000000000Z",
		journalFile + ".unreadable-20260103T000000.000000000Z"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cleanupErr := errors.New("cleanup directory sync failed")
	syncs := 0
	restore := fsq.SyncDirAmbientSwapForTest(func(d string) error {
		if d == dir {
			syncs++
			if syncs == 2 {
				return cleanupErr
			}
		}
		return nil
	})
	defer restore()
	kept, err := store.SetAsideJournal()
	if kept == "" || !errors.Is(err, cleanupErr) {
		t.Fatalf("recovery path %q, error %v; want a kept journal and cleanup diagnostic", kept, err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal(err)
	}
	if j, err := store.LoadJournal(); err != nil || len(j.Attempts) != 0 {
		t.Fatalf("journal after recovery = %+v, %v; want an empty usable journal", j, err)
	}
}
