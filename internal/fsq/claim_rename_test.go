package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func setupClaimAgent(t *testing.T, agent string) string {
	t.Helper()
	base := t.TempDir()
	if err := EnsureAgentDirs(base, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	return base
}

func writeClaimMessage(t *testing.T, base, agent, filename string) {
	t.Helper()
	path := filepath.Join(AgentInboxNew(base, agent), filename)
	if err := os.WriteFile(path, []byte("claim test payload"), 0o600); err != nil {
		t.Fatalf("write inbox message: %v", err)
	}
}

func TestMoveNewToCurWinnerClaimsExactlyOnce(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	writeClaimMessage(t, base, "alice", "claim_once.md")

	root := openDeliveryRootForTest(t, base)
	if err := MoveNewToCur(root, "alice", "claim_once.md"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := MoveNewToCur(root, "alice", "claim_once.md")
	if err == nil {
		t.Fatal("second claim succeeded; claim must be exclusive")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("second claim error = %T %v, want os.IsNotExist", err, err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxCur(base, "alice"), "claim_once.md")); statErr != nil {
		t.Fatalf("claimed message missing from cur: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(base, "alice"), "claim_once.md")); !os.IsNotExist(statErr) {
		t.Fatalf("claimed message still present in new: %v", statErr)
	}
}

// TestMoveNewToCurConcurrentClaimsSingleWinner releases many claimers at once
// and requires exactly one winner. This is the fsq-level twin of the CLI-level
// drain regression test that fails on windows/amd64 in issue #485.
func TestMoveNewToCurConcurrentClaimsSingleWinner(t *testing.T) {
	const claimers = 8
	base := setupClaimAgent(t, "alice")
	writeClaimMessage(t, base, "alice", "claim_race.md")

	roots := make([]*DeliveryRoot, claimers)
	for i := range roots {
		roots[i] = openDeliveryRootForTest(t, base)
	}

	start := make(chan struct{})
	results := make(chan error, claimers)
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(root *DeliveryRoot) {
			defer wg.Done()
			<-start
			results <- MoveNewToCur(root, "alice", "claim_race.md")
		}(roots[i])
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case os.IsNotExist(err):
		default:
			var committed *CommittedDurabilityError
			if errors.As(err, &committed) {
				winners++
				continue
			}
			t.Fatalf("claim error = %T %v, want nil or os.IsNotExist", err, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxCur(base, "alice"), "claim_race.md")); err != nil {
		t.Fatalf("claimed message missing from cur: %v", err)
	}
}

func TestMoveNewToCurLoserForMissingMessage(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	root := openDeliveryRootForTest(t, base)
	err := MoveNewToCur(root, "alice", "never_delivered.md")
	if err == nil {
		t.Fatal("claim of missing message succeeded")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("error = %T %v, want os.IsNotExist", err, err)
	}
}
