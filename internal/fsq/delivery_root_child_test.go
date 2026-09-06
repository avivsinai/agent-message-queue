package fsq

import (
	"errors"
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

func TestCreateDirectChildExclusiveFailsIfNameExists(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := openDeliveryRootForTest(t, base)
	_, err := root.CreateDirectChildExclusive("auth", 0o700)
	if err == nil {
		t.Fatal("expected exists error")
	}
	var exists *DirectChildExistsError
	if !errors.As(err, &exists) || exists.Name != "auth" {
		t.Fatalf("error = %v, want DirectChildExistsError", err)
	}
}

func TestCreateDirectChildExclusiveReportsCommittedDurabilityFailure(t *testing.T) {
	base := t.TempDir()
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	failure := errors.New("sync parent failed")
	root.syncDirForTest = func(string) error { return failure }
	child, err := root.CreateDirectChildExclusive("profile-a", 0o700)
	if child == nil {
		t.Fatal("committed child capability is missing")
	}
	defer func() { _ = child.Close() }()
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, failure) {
		t.Fatalf("CreateDirectChildExclusive error = %v", err)
	}
	info, statErr := os.Stat(filepath.Join(root.Base(), "profile-a"))
	if statErr != nil || !info.IsDir() {
		t.Fatalf("committed child is not visible: info=%v err=%v", info, statErr)
	}
}

func TestPublishInitializedDirectChildExclusiveIsAllOrNothing(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	failure := errors.New("initializer failed")
	if _, err := root.PublishInitializedDirectChildExclusive("collab", 0o700, func(child *DeliveryRoot) error {
		if err := child.EnsureRootDirs(); err != nil {
			return err
		}
		if err := child.EnsureAgentDirs("operator"); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("failed publication error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "collab")); !os.IsNotExist(err) {
		t.Fatalf("failed publication exposed authoritative child: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed publication left staging entries: %v", entries)
	}

	child, err := root.PublishInitializedDirectChildExclusive("collab", 0o700, func(child *DeliveryRoot) error {
		if err := child.EnsureRootDirs(); err != nil {
			return err
		}
		return child.EnsureAgentDirs("operator")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	if err := child.VerifyBase(); err != nil {
		t.Fatal(err)
	}
	for _, path := range requiredMailboxLeaves {
		if info, err := child.Stat(filepath.Join("agents", "operator", string(path))); err != nil || !info.IsDir() {
			t.Fatalf("published mailbox path %s: info=%v err=%v", path, info, err)
		}
	}
}
