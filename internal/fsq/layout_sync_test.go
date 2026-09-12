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

	// Every created level, leaf first, and then the pre-existing parent whose
	// directory entry changed. fsync(d) persists d's entries, never d's own
	// entry in its parent — so "sync only what we created" (the earlier
	// version) left the new subtree's entry in the existing parent unsynced.
	// Here the root's base dir pre-exists, so the parent of "agents" is the
	// root and is covered by the base guarantees; the chain is exactly the
	// four created levels.
	want := []string{
		filepath.Join("agents", "agent-a", "inbox", "cur"),
		filepath.Join("agents", "agent-a", "inbox"),
		filepath.Join("agents", "agent-a"),
		"agents",
	}
	if len(synced) != len(want) {
		t.Fatalf("synced %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != want[i] {
			t.Fatalf("sync order %v, want %v", synced, want)
		}
	}

	// With "agents" pre-existing, a second agent creates 3 levels — and the
	// existing "agents" directory (which now owns the new entry) MUST be
	// synced too, even though this call did not create it.
	synced = nil
	if err := root.mkdirAllSynced(filepath.Join("agents", "agent-b", "inbox", "cur")); err != nil {
		t.Fatalf("mkdirAllSynced (second agent): %v", err)
	}
	want = []string{
		filepath.Join("agents", "agent-b", "inbox", "cur"),
		filepath.Join("agents", "agent-b", "inbox"),
		filepath.Join("agents", "agent-b"),
		"agents", // pre-existing parent whose entry changed
	}
	if len(synced) != len(want) {
		t.Fatalf("second agent synced %v, want %v (existing parent must be synced)", synced, want)
	}
	for i := range want {
		if synced[i] != want[i] {
			t.Fatalf("second agent sync order %v, want %v", synced, want)
		}
	}

	// An established tree is not re-synced here: tree durability belongs to
	// creation, message durability to the delivery commit (which syncs new).
	synced = nil
	if err := root.mkdirAllSynced(filepath.Join("agents", "agent-b", "inbox", "cur")); err != nil {
		t.Fatalf("mkdirAllSynced (existing tree): %v", err)
	}
	if len(synced) != 0 {
		t.Fatalf("existing tree must not be re-synced by mkdir, got %v", synced)
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
	var syncedAgents atomic.Bool
	restore := syncDirAmbientSwapForTest(func(dir string) error {
		calls.Add(1)
		if filepath.Base(dir) == "agents" {
			syncedAgents.Store(true)
		}
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
	// Every leaf's chain reaches the pre-existing "agents" directory, whose
	// entry for agent-b changed: it must be synced at least once, and the
	// call must terminate (before the descend-from-leaf fix it never did).
	leaves := RequiredMailboxLeaves()
	if calls.Load() < int64(len(leaves)) {
		t.Fatalf("ambient sync calls = %d, want at least one per leaf (%d)", calls.Load(), len(leaves))
	}
	if !syncedAgents.Load() {
		t.Fatal("pre-existing agents/ directory (owner of the new agent-b entry) was never synced")
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

// TestDeliverToInboxesSyncsFirstContactMailboxTree reproduces
// agent-message-queue-611.22.26 (fsq first-contact sync): DeliverToInboxes
// built agents/<h>/inbox/{tmp,new} with a raw MkdirAll and fsynced only the
// tmp and new leaves, so the agents/<h> and inbox directory entries were
// never durable. `amq send --to newagent` is a first-contact delivery (send
// and reply never provision); power loss after "sent" lost the mailbox and
// the committed message. Every level this delivery creates must be synced.
func TestDeliverToInboxesSyncsFirstContactMailboxTree(t *testing.T) {
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

	synced := map[string]int{}
	root.syncDirForTest = func(dir string) error {
		rel, relErr := filepath.Rel(base, dir)
		if relErr != nil {
			rel = dir
		}
		synced[rel]++
		return nil
	}

	if _, err := DeliverToInboxes(root, []string{"newagent"}, "m.md", []byte("hello")); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Every level whose ENTRIES changed must be synced: the created
	// ancestors AND "agents" (pre-existing, but it now owns the newagent
	// entry). "inbox" changes twice — once when tmp is created and again when
	// new is — so it must be synced after the second creation too; asserting
	// "exactly once" would accept a sync that happened before new existed
	// (the defect the first version of this test encoded).
	for _, dir := range []string{
		"agents",
		filepath.Join("agents", "newagent"),
		filepath.Join("agents", "newagent", "inbox"),
	} {
		if synced[dir] == 0 {
			t.Fatalf("mailbox level %s was never synced (all: %v)", dir, synced)
		}
	}
	if synced[filepath.Join("agents", "newagent", "inbox")] < 2 {
		t.Fatalf("inbox must be synced after each child (tmp, new) is created; got %d (all: %v)", synced[filepath.Join("agents", "newagent", "inbox")], synced)
	}
	for _, leaf := range []string{
		filepath.Join("agents", "newagent", "inbox", "tmp"),
		filepath.Join("agents", "newagent", "inbox", "new"),
	} {
		if synced[leaf] == 0 {
			t.Fatalf("leaf %s was never synced (all: %v)", leaf, synced)
		}
	}
}
