//go:build darwin || linux

package fsq

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDeliverToExistingInboxKeepsCommitInOpenedRootAfterAliasSwap(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "peer")
	parked := filepath.Join(parent, "peer-parked")
	outside := filepath.Join(parent, "outside")
	for _, tree := range []string{base, outside} {
		if err := EnsureAgentDirs(tree, "codex"); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", tree, err)
		}
	}

	root := openDeliveryRootForTest(t, base)
	tmpDir := filepath.Join("agents", "codex", "inbox", "tmp")
	reachedCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	var once sync.Once
	root.syncDirForTest = func(dir string) error {
		if dir == tmpDir {
			once.Do(func() {
				close(reachedCommit)
				<-releaseCommit
			})
		}
		return root.syncDirPlatform(dir)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := DeliverToExistingInbox(root, "codex", "cross-project.md", []byte("contained"))
		errCh <- err
	}()
	<-reachedCommit
	if err := os.Rename(base, parked); err != nil {
		t.Fatalf("park authorized peer root: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("replace peer root alias: %v", err)
	}
	close(releaseCommit)

	if err := <-errCh; err != nil {
		t.Fatalf("DeliverToExistingInbox through opened root: %v", err)
	}
	assertMailboxCount(t, filepath.Join(parked, "agents", "codex", "inbox", "new"), 1)
	assertMailboxEmpty(t, filepath.Join(parked, "agents", "codex", "inbox", "tmp"))
	assertMailboxEmpty(t, filepath.Join(outside, "agents", "codex", "inbox", "new"))
}

func assertMailboxEmpty(t *testing.T, dir string) {
	t.Helper()
	assertMailboxCount(t, dir, 0)
}

func assertMailboxCount(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != want {
		t.Fatalf("ReadDir(%s) count = %d, want %d", dir, len(entries), want)
	}
}
