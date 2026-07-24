//go:build darwin || linux

package fsq

import (
	"errors"
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

func TestDeliverToInboxesKeepsRemainingCommitsInOpenedRootAfterAliasSwap(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	outside := filepath.Join(parent, "outside")
	for _, tree := range []string{base, outside} {
		for _, agent := range []string{"alice", "bob"} {
			if err := EnsureAgentDirs(tree, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s,%s): %v", tree, agent, err)
			}
		}
	}

	root := openDeliveryRootForTest(t, base)
	firstNewDir := filepath.Join("agents", "alice", "inbox", "new")
	firstCommitted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var once sync.Once
	root.syncDirForTest = func(dir string) error {
		if dir == firstNewDir {
			once.Do(func() {
				close(firstCommitted)
				<-releaseSecond
			})
		}
		return root.syncDirPlatform(dir)
	}

	type result struct {
		paths map[string]string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		paths, err := DeliverToInboxes(root, []string{"alice", "bob"}, "batch.md", []byte("contained"))
		resultCh <- result{paths: paths, err: err}
	}()
	<-firstCommitted
	if err := os.Rename(base, parked); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("replace authorized root alias: %v", err)
	}
	close(releaseSecond)

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("DeliverToInboxes through opened root: %v", got.err)
	}
	if len(got.paths) != 2 {
		t.Fatalf("DeliverToInboxes paths = %#v, want two committed recipients", got.paths)
	}

	assertMailboxCount(t, filepath.Join(parked, "agents", "alice", "inbox", "new"), 1)
	assertMailboxCount(t, filepath.Join(parked, "agents", "bob", "inbox", "new"), 1)
	assertMailboxEmpty(t, filepath.Join(parked, "agents", "bob", "inbox", "tmp"))
	assertMailboxEmpty(t, filepath.Join(outside, "agents", "alice", "inbox", "new"))
	assertMailboxEmpty(t, filepath.Join(outside, "agents", "bob", "inbox", "new"))
}

func TestDeliverToInboxesRollsBackAfterEscapingNewDirSwapBetweenCommits(t *testing.T) {
	base := t.TempDir()
	for _, agent := range []string{"alice", "bob", "carol"} {
		if err := EnsureAgentDirs(base, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	outside := t.TempDir()

	root := openDeliveryRootForTest(t, base)
	firstNewDir := filepath.Join("agents", "alice", "inbox", "new")
	firstCommitted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var once sync.Once
	root.syncDirForTest = func(dir string) error {
		if dir == firstNewDir {
			once.Do(func() {
				close(firstCommitted)
				<-releaseSecond
			})
		}
		return root.syncDirPlatform(dir)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := DeliverToInboxes(root, []string{"alice", "bob", "carol"}, "escape.md", []byte("contained"))
		errCh <- err
	}()
	<-firstCommitted
	bobNew := AgentInboxNew(base, "bob")
	if err := os.RemoveAll(bobNew); err != nil {
		t.Fatalf("remove bob new: %v", err)
	}
	if err := os.Symlink(outside, bobNew); err != nil {
		t.Fatalf("replace bob new with escaping symlink: %v", err)
	}
	close(releaseSecond)

	err := <-errCh
	if err == nil {
		t.Fatal("DeliverToInboxes succeeded through an escaping new-dir symlink")
	}
	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("DeliverToInboxes error = %T %v, want PartialDeliveryError", err, err)
	}
	if partial.Failed != "bob" {
		t.Fatalf("Failed = %q, want bob", partial.Failed)
	}
	if len(partial.Pending) != 1 || partial.Pending[0] != "carol" {
		t.Fatalf("Pending = %#v, want [carol]", partial.Pending)
	}
	if _, ok := partial.Delivered["alice"]; !ok {
		t.Fatalf("Delivered = %#v, want alice", partial.Delivered)
	}
	assertMailboxEmpty(t, outside)
	assertMailboxEmpty(t, AgentInboxTmp(base, "bob"))
	assertMailboxEmpty(t, AgentInboxTmp(base, "carol"))
	assertMailboxEmpty(t, AgentInboxNew(base, "carol"))
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
