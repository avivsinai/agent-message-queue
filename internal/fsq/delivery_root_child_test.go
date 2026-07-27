package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryRootOpenOrCreateDirectChildRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "collab")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if child, err := root.OpenOrCreateDirectChild("collab", 0o700); err == nil {
		_ = child.Close()
		t.Fatal("expected direct-child symlink refusal")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("direct-child open mutated symlink target: %v", entries)
	}
}

func TestDeliveryRootDirectChildProvisioningFailsClosedAfterAliasSwap(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "collab"), 0o700); err != nil {
		t.Fatal(err)
	}

	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	child, err := root.OpenOrCreateDirectChild("collab", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()

	if err := os.Rename(filepath.Join(base, "collab"), filepath.Join(base, "parked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "collab")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := child.EnsureAgentDirs("alice"); err == nil {
		t.Fatal("expected changed child alias to fail closed")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("pinned child provisioning mutated swapped symlink target: %v", entries)
	}
}
