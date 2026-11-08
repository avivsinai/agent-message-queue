package sessionguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Regression: the pin was checked against a path and the
// update then re-resolved the path, so a directory swapped in between took
// the write. The capability itself must be what the pin authenticates.
func TestVerifyEnvPinOnChecksTheOpenCapability(t *testing.T) {
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	root := filepath.Join(base, "s")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rootID, err := fsq.StableTreeIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "s")
	t.Setenv(EnvRootID, rootID)
	t.Setenv(EnvBaseRootID, baseID)
	snap, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := fsq.OpenDeliveryRoot(root, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	// Swap a different directory in at the path after the capability opened.
	if err := os.Rename(root, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnvPinOn(dr); err != nil {
		t.Fatalf("the authenticated capability was refused because the path changed: %v", err)
	}
}
