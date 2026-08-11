//go:build windows

package fsq

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWindowsClaimRenameConcurrentSingleWinner(t *testing.T) {
	base := t.TempDir()
	if err := EnsureRootDirs(base); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	root := openDeliveryRootForTest(t, base)
	const filename = "windows-exclusive-claim.md"
	payload := []byte("one physical message\n")
	if _, err := DeliverToInbox(root, "alice", filename, payload); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}

	newPath := filepath.Join("agents", "alice", "inbox", "new", filename)
	curPath := filepath.Join("agents", "alice", "inbox", "cur", filename)
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			<-start
			results <- claimRename(root, newPath, curPath)
		}()
	}
	close(start)
	group.Wait()
	close(results)

	winners := 0
	losers := 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case os.IsNotExist(err):
			losers++
		default:
			t.Fatalf("claimRename returned unexpected error: %T %v", err, err)
		}
	}
	if winners != 1 || losers != workers-1 {
		t.Fatalf("claim outcomes: winners=%d losers=%d, want 1/%d", winners, losers, workers-1)
	}
	if _, err := os.Stat(filepath.Join(base, newPath)); !os.IsNotExist(err) {
		t.Fatalf("source still visible after winning claim: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, curPath))
	if err != nil {
		t.Fatalf("read claimed payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("claimed payload = %q, want %q", got, payload)
	}
}
