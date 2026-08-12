//go:build windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func stubRemoveClaimSource(t *testing.T, fn func(source windows.Handle) error) {
	t.Helper()
	old := removeClaimSource
	removeClaimSource = fn
	t.Cleanup(func() { removeClaimSource = old })
}

// unlinkClaimSourceOutOfBand removes the source name through an independent
// POSIX-disposition handle — the actual production primitive — simulating a
// concurrent claimant winning the inner disposition race. os.Remove cannot
// stand in here: it uses classic DeleteFile delete-on-close, which leaves the
// name visible while the claimant's handle is still open. Closing the
// independent handle removes the namespace link while other existing handles
// remain usable.
func unlinkClaimSourceOutOfBand(t *testing.T, path string) {
	t.Helper()
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open source directory: %v", err)
	}
	defer func() { _ = dir.Close() }()
	handle, err := openClaimSource(windows.Handle(dir.Fd()), filepath.Base(path))
	if err != nil {
		t.Fatalf("open independent claim handle: %v", err)
	}
	if err := setClaimSourceDisposition(handle); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("posix unlink via independent handle: %v", err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatalf("close independent handle: %v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("source name state after out-of-band unlink = %v, want absent", statErr)
	}
}

// A residue-reconciling loser whose removal fails with a status outside
// claimTransitionAlreadyDone must still honor the loser contract when the
// source name is provably gone. This is the race behind the flaky
// TestMoveNewToCurConcurrentClaimsSingleWinner Access-is-denied failure.
func TestClaimResidueLoserBenignWhenSourceAlreadyUnlinked(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), "residue.md")
	curPath := filepath.Join(AgentInboxCur(base, "alice"), "residue.md")
	if err := os.WriteFile(newPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Winner residue state: cur linked, new still present, same file.
	if err := os.Link(newPath, curPath); err != nil {
		t.Fatal(err)
	}

	stubRemoveClaimSource(t, func(source windows.Handle) error {
		unlinkClaimSourceOutOfBand(t, newPath)
		return windows.STATUS_ACCESS_DENIED
	})

	root := openDeliveryRootForTest(t, base)
	err := MoveNewToCur(root, "alice", "residue.md")
	if !os.IsNotExist(err) {
		t.Fatalf("error = %T %v, want os.IsNotExist loser contract", err, err)
	}
	if got, readErr := os.ReadFile(curPath); readErr != nil || string(got) != "body" {
		t.Fatalf("retained cur copy = %q, %v; want intact payload", got, readErr)
	}
	if _, statErr := os.Lstat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("source name state = %v, want absent", statErr)
	}
}

// The same adjudication on the winner path: a committed claim whose source
// removal reports a hostile status must not surface residue durability noise
// when the name is already gone.
func TestClaimWinnerBenignWhenSourceAlreadyUnlinked(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), "winner.md")
	if err := os.WriteFile(newPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}

	stubRemoveClaimSource(t, func(source windows.Handle) error {
		unlinkClaimSourceOutOfBand(t, newPath)
		return windows.STATUS_ACCESS_DENIED
	})

	root := openDeliveryRootForTest(t, base)
	if err := MoveNewToCur(root, "alice", "winner.md"); err != nil {
		t.Fatalf("winner claim = %v, want nil", err)
	}
	curPath := filepath.Join(AgentInboxCur(base, "alice"), "winner.md")
	if got, readErr := os.ReadFile(curPath); readErr != nil || string(got) != "body" {
		t.Fatalf("claimed cur copy = %q, %v; want intact payload", got, readErr)
	}
	if _, statErr := os.Lstat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("source name state = %v, want absent", statErr)
	}
}

// A removal failure with the source name still present must stay loud at both
// sites; proven absence is the only benign evidence. This is the case that
// proves real DELETE-access failures are not masked.
func TestClaimWinnerRemovalFailureWithSourcePresentStaysLoud(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), "loud.md")
	if err := os.WriteFile(newPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}

	stubRemoveClaimSource(t, func(source windows.Handle) error {
		return windows.STATUS_ACCESS_DENIED
	})

	root := openDeliveryRootForTest(t, base)
	err := MoveNewToCur(root, "alice", "loud.md")
	if err == nil || os.IsNotExist(err) {
		t.Fatalf("error = %T %v, want loud residue failure", err, err)
	}
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("error = %T %v, want CommittedDurabilityError", err, err)
	}
	if _, statErr := os.Lstat(newPath); statErr != nil {
		t.Fatalf("source name state = %v, want still present", statErr)
	}
}

func TestClaimResidueLoserRemovalFailureWithSourcePresentStaysLoud(t *testing.T) {
	base := setupClaimAgent(t, "alice")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), "loud_residue.md")
	curPath := filepath.Join(AgentInboxCur(base, "alice"), "loud_residue.md")
	if err := os.WriteFile(newPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(newPath, curPath); err != nil {
		t.Fatal(err)
	}

	stubRemoveClaimSource(t, func(source windows.Handle) error {
		return windows.STATUS_ACCESS_DENIED
	})

	root := openDeliveryRootForTest(t, base)
	err := MoveNewToCur(root, "alice", "loud_residue.md")
	if err == nil || os.IsNotExist(err) {
		t.Fatalf("error = %T %v, want loud remove-duplicate failure", err, err)
	}
	if _, statErr := os.Lstat(newPath); statErr != nil {
		t.Fatalf("source name state = %v, want still present", statErr)
	}
}
