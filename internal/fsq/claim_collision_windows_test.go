//go:build windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestClaimCollisionPreservesBothCopies proves the loud-collision contract:
// when inbox/new and inbox/cur both hold the claimed filename, the claim is
// refused with *ClaimCollisionError and neither copy is modified. Unix
// deliberately keeps rename-replace semantics (the state is unreachable
// through AMQ's own flows), so this contract is Windows-only.
func TestClaimCollisionPreservesBothCopies(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	newPath := filepath.Join(AgentInboxNew(base, "alice"), "collide.md")
	curPath := filepath.Join(AgentInboxCur(base, "alice"), "collide.md")
	if err := os.WriteFile(newPath, []byte("new copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(curPath, []byte("retained cur copy"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)
	err := MoveNewToCur(root, "alice", "collide.md")
	var collision *ClaimCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error = %T %v, want *ClaimCollisionError", err, err)
	}

	got, readErr := os.ReadFile(curPath)
	if readErr != nil || string(got) != "retained cur copy" {
		t.Fatalf("cur copy = %q, %v; collision must not overwrite the retained copy", got, readErr)
	}
	got, readErr = os.ReadFile(newPath)
	if readErr != nil || string(got) != "new copy" {
		t.Fatalf("new copy = %q, %v; collision must not consume the source", got, readErr)
	}
}

// TestClaimPinnedRootIgnoresBaseAliasSwap is the capability acceptance
// property from the issue #485 design review: after DeliveryRoot pins its
// base, replacing the lexical base path must not redirect the claim. The
// primitive must mutate the originally pinned tree and leave the impostor
// tree byte-identical — exclusivity gained by discarding root authority
// would be an incomplete fix.
func TestClaimPinnedRootIgnoresBaseAliasSwap(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "root")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	newRel := filepath.Join("agents", "alice", "inbox", "new", "pinned.md")
	curRel := filepath.Join("agents", "alice", "inbox", "cur", "pinned.md")
	if err := os.WriteFile(filepath.Join(base, newRel), []byte("pinned payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)

	// Swap the lexical base for an impostor tree carrying a decoy message.
	moved := filepath.Join(parent, "root.moved")
	if err := os.Rename(base, moved); err != nil {
		t.Fatalf("alias-swap rename: %v", err)
	}
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs impostor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, newRel), []byte("decoy payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Call the primitive directly: VerifyBase in MoveNewToCur intentionally
	// fails closed on a moved base, but the primitive itself must also be
	// pinned so no caller ordering can be redirected.
	if err := claimRename(root, newRel, curRel); err != nil {
		t.Fatalf("claimRename through pinned root: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(moved, curRel))
	if err != nil || string(got) != "pinned payload" {
		t.Fatalf("pinned tree cur = %q, %v; claim must land in the originally pinned tree", got, err)
	}
	if _, err := os.Stat(filepath.Join(moved, newRel)); !os.IsNotExist(err) {
		t.Fatalf("pinned tree new still present: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(base, newRel))
	if err != nil || string(got) != "decoy payload" {
		t.Fatalf("impostor new = %q, %v; impostor tree must stay untouched", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, curRel)); !os.IsNotExist(err) {
		t.Fatalf("impostor cur was created: %v", err)
	}
}
