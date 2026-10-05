package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupRemoteEventsRemovesOldKeepsNew: an old record is removed and a
// new one is kept; --dry-run removes nothing; a lock newer than the cutoff is
// skipped. (agent-message-queue-3qc)
func TestCleanupRemoteEventsRemovesOldKeepsNew(t *testing.T) {
	root := t.TempDir()
	eventsDir := filepath.Join(root, "meta", "acp", "remote-events")
	if err := os.MkdirAll(eventsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	newer := time.Now().Add(-time.Minute)
	mk := func(name string, mtime time.Time) {
		p := filepath.Join(eventsDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	mk("ev-old.json", old)
	mk("ev-old.mailbox.json", old)
	mk("ev-old.mailbox.lock", old)
	mk("ev-old.cancelled", old)
	mk("ev-new.json", newer)
	mk("ev-hold.mailbox.lock", newer) // a held (fresh) lock: must survive
	// a non-regular entry and a symlink: never touched
	if err := os.MkdirAll(filepath.Join(eventsDir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ev-new.json", filepath.Join(eventsDir, "ev-link")); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)

	// dry run: names the old files, removes nothing
	dry, err := CleanupRemoteEvents(eventsDir, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	wantDry := []string{"ev-old.cancelled", "ev-old.json", "ev-old.mailbox.json", "ev-old.mailbox.lock"}
	if !equalStrings(dry.Removed, wantDry) {
		t.Fatalf("dry run Removed = %v, want %v", dry.Removed, wantDry)
	}
	if !equalStrings(dry.Skipped, []string{"ev-hold.mailbox.lock"}) {
		t.Fatalf("dry run Skipped = %v, want the fresh lock", dry.Skipped)
	}
	if _, err := os.Stat(filepath.Join(eventsDir, "ev-old.json")); err != nil {
		t.Fatalf("dry run must not remove: %v", err)
	}

	// real run: old gone, new + held lock + symlink kept
	rep, err := CleanupRemoteEvents(eventsDir, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(rep.Removed, wantDry) {
		t.Fatalf("Removed = %v, want %v", rep.Removed, wantDry)
	}
	for _, gone := range wantDry {
		if _, err := os.Lstat(filepath.Join(eventsDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err = %v", gone, err)
		}
	}
	for _, kept := range []string{"ev-new.json", "ev-hold.mailbox.lock", "ev-link", "subdir"} {
		if _, err := os.Lstat(filepath.Join(eventsDir, kept)); err != nil {
			t.Fatalf("expected %s kept: %v", kept, err)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
