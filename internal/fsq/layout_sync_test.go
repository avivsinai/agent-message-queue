package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// mkdirAllSynced and the ambient EnsureAgentDirs helper must sync the leaf of
// the newly created mailbox tree first and then each remaining created level
// up to the shallowest missing ancestor. firstMissingAncestor reports the
// shallowest missing level, so an implementation that walks up from it (or
// starts there) never reaches the leaf and either loops forever or skips the
// sync that protects the newly committed message file.
func TestMkdirAllSyncedCoversEveryCreatedLevelDownToLeaf(t *testing.T) {
	base := t.TempDir()
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	var synced []string
	root.syncDirForTest = func(dir string) error {
		rel, relErr := filepath.Rel(base, dir)
		if relErr != nil {
			rel = dir
		}
		synced = append(synced, rel)
		return nil
	}

	if err := root.mkdirAllSynced(filepath.Join("agents", "agent-a", "inbox", "cur")); err != nil {
		t.Fatalf("mkdirAllSynced: %v", err)
	}

	want := map[string]bool{
		filepath.Join("agents", "agent-a", "inbox", "cur"): false,
		filepath.Join("agents", "agent-a", "inbox"):        false,
		filepath.Join("agents", "agent-a"):                 false,
		"agents":                                           false,
	}
	if len(synced) != len(want) {
		t.Fatalf("synced %d directories (%v), want %d", len(synced), synced, len(want))
	}
	for _, dir := range synced {
		seen, ok := want[dir]
		if !ok {
			t.Fatalf("unexpected directory synced: %s (all: %v)", dir, synced)
		}
		if seen {
			t.Fatalf("directory synced more than once: %s (all: %v)", dir, synced)
		}
		want[dir] = true
	}
	if synced[0] != filepath.Join("agents", "agent-a", "inbox", "cur") {
		t.Fatalf("leaf must be synced first, got %v", synced)
	}
}

// The ambient EnsureAgentDirs helper shares firstMissingAncestorAmbient and
// must terminate: before the descend-from-leaf fix it fsynced up from the
// shallowest missing level and never broke, hanging any caller.
func TestEnsureAgentDirsAmbientSyncTerminatesAndCoversCreatedLevels(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}

	calls := atomic.Int64{}
	restore := syncDirAmbientSwapForTest(func(dir string) error {
		calls.Add(1)
		if calls.Load() > 32 {
			t.Errorf("ambient sync exceeded expected level count (infinite loop?): %d calls", calls.Load())
			return errors.New("sync loop guard")
		}
		return nil
	})
	defer restore()

	if err := EnsureAgentDirs(root, "agent-b"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	// First leaf creates agent-b + inbox + tmp = 3 synced levels (agents is
	// pre-created by EnsureRootDirs); outbox/sent and dlq/tmp add one extra
	// level each; the remaining leaves already have their parents. So the
	// total is len(leaves) + 4 created levels, leaf-first, never repeating.
	leaves := RequiredMailboxLeaves()
	if got := calls.Load(); got != int64(len(leaves)+4) {
		t.Fatalf("ambient sync calls = %d, want %d", got, len(leaves)+4)
	}
	for _, leaf := range leaves {
		info, err := os.Stat(AgentMailboxPath(root, "agent-b", leaf))
		if err != nil || !info.IsDir() {
			t.Fatalf("leaf %s not created: info=%v err=%v", leaf, info, err)
		}
	}
}

// A failed sync on a created level is a plain error (nothing committed at the
// leaf yet), not a swallowed success.
func TestMkdirAllSyncedReportsFailedAncestorSync(t *testing.T) {
	base := t.TempDir()
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	failure := errors.New("sync ancestor failed")
	root.syncDirForTest = func(dir string) error { return failure }

	err = root.mkdirAllSynced(filepath.Join("agents", "agent-c", "inbox", "cur"))
	if !errors.Is(err, failure) {
		t.Fatalf("mkdirAllSynced error = %v, want wrapped %v", err, failure)
	}
}
