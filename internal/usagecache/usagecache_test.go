package usagecache

import (
	"errors"
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
