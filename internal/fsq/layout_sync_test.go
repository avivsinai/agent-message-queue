package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// mkdirAllSynced must fsync every directory that owns an entry on the way to
// the new mailbox leaf, up to and including the pinned root, and it must do so
// on every call. An fsync persists a directory's entries and never its own
// entry in its parent, so stopping short of the root leaves the root's entry
// for a brand-new agents/ subtree unsynced; and an existing directory is not a
// durable one, because an earlier call may have created it and then died
// before syncing the parent. Both were BLOCK findings on
// agent-message-queue-611.22.26 (fsq first-contact sync).
func TestMkdirAllSyncedCoversEveryOwningDirectoryUpToRoot(t *testing.T) {
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

	// The chain: the leaf's parent, then upward, ending at the pinned root.
	// The leaf itself is absent — it is empty until the delivery commit, which
	// syncs it after the rename.
	want := []string{
		filepath.Join("agents", "agent-a", "inbox"),
		filepath.Join("agents", "agent-a"),
		"agents",
		".",
	}
	assertSync := func(label string, got []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s synced %v, want %v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s sync order %v, want %v", label, got, want)
			}
		}
	}

	if err := root.mkdirAllSynced(filepath.Join("agents", "agent-a", "inbox", "cur")); err != nil {
		t.Fatalf("mkdirAllSynced: %v", err)
	}
	assertSync("first contact", synced)

	// The same tree already exists. It is still re-synced: Stat proves
	// existence, not durability.
	synced = nil
	if err := root.mkdirAllSynced(filepath.Join("agents", "agent-a", "inbox", "cur")); err != nil {
		t.Fatalf("mkdirAllSynced (existing tree): %v", err)
	}
	assertSync("existing tree", synced)
}

// Sibling directories of one mailbox share their ancestors. mkdirAllSynced
// creates them all before it syncs, so a shared parent is never fsynced while
// a later sibling below it still has no durable entry — and it is fsynced
// once, not once per sibling.
func TestMkdirAllSyncedSyncsSharedAncestorsAfterEverySibling(t *testing.T) {
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

	inbox := filepath.Join("agents", "agent-a", "inbox")
	counts := map[string]int{}
	var siblingsAtInboxSync int
	root.syncDirForTest = func(dir string) error {
		rel, relErr := filepath.Rel(base, dir)
		if relErr != nil {
			rel = dir
		}
		counts[rel]++
		if rel == inbox {
			entries, readErr := root.ReadDir(inbox)
			if readErr != nil {
				t.Errorf("ReadDir(%s): %v", inbox, readErr)
			}
			siblingsAtInboxSync = len(entries)
		}
		return nil
	}

	if err := root.mkdirAllSynced(filepath.Join(inbox, "tmp"), filepath.Join(inbox, "new")); err != nil {
		t.Fatalf("mkdirAllSynced: %v", err)
	}
	if siblingsAtInboxSync != 2 {
		t.Fatalf("inbox was synced with %d children, want 2 (tmp and new must both exist first)", siblingsAtInboxSync)
	}
	if counts[inbox] != 1 {
		t.Fatalf("inbox synced %d times, want 1 (shared ancestors are synced once)", counts[inbox])
	}
}

// The ambient EnsureAgentDirs twin must terminate (it once fsynced upward from
// the shallowest missing level and never broke, hanging the caller) and must
// cover the same owning directories, including the pre-existing agents/ and
// the queue root that own the new agent's entries.
func TestEnsureAgentDirsAmbientSyncTerminatesAndCoversOwningDirectories(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}

	calls := atomic.Int64{}
	synced := map[string]bool{}
	restore := syncDirAmbientSwapForTest(func(dir string) error {
		if calls.Add(1) > 64 {
			return errors.New("sync loop guard")
		}
		synced[dir] = true
		return nil
	})
	defer restore()

	if err := EnsureAgentDirs(root, "agent-b"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	agent := AgentBase(root, "agent-b")
	for _, dir := range []string{
		filepath.Join(agent, "inbox"),
		filepath.Join(agent, "outbox"),
		filepath.Join(agent, "dlq"),
		agent,
		filepath.Join(root, "agents"), // pre-existing, but it owns the new agent-b entry
		root,
	} {
		if !synced[dir] {
			t.Fatalf("owning directory %s was never synced", dir)
		}
	}
	if got := calls.Load(); got != int64(len(synced)) {
		t.Fatalf("ambient sync made %d calls for %d directories; shared ancestors must be synced once", got, len(synced))
	}
	for _, leaf := range RequiredMailboxLeaves() {
		info, err := os.Stat(AgentMailboxPath(root, "agent-b", leaf))
		if err != nil || !info.IsDir() {
			t.Fatalf("leaf %s not created: info=%v err=%v", leaf, info, err)
		}
	}
}

// A failed sync on an owning directory is a plain error (nothing committed at
// the leaf yet), not a swallowed success.
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
// the committed message.
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

	// Every directory that owns an entry of the new tree: the created
	// ancestors, plus "agents" and the pinned root, which pre-exist but now
	// own the newagent entry and the agents entry respectively.
	for _, dir := range []string{
		".",
		"agents",
		filepath.Join("agents", "newagent"),
		filepath.Join("agents", "newagent", "inbox"),
	} {
		if synced[dir] == 0 {
			t.Fatalf("mailbox level %s was never synced (all: %v)", dir, synced)
		}
	}
	// The message's own leaf is synced by the delivery commit after the
	// rename, not by the mailbox creation.
	if synced[filepath.Join("agents", "newagent", "inbox", "new")] == 0 {
		t.Fatalf("committed leaf was never synced (all: %v)", synced)
	}
}
