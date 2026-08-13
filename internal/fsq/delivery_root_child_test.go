package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestDeliveryRootPinnedBatchExpiresAfterCallback(t *testing.T) {
	root := openDeliveryRootForTest(t, t.TempDir())
	var retained *DeliveryRoot

	if err := root.WithPinnedBatch(func(batch *DeliveryRoot) error {
		retained = batch
		return batch.EnsureRootDirs()
	}); err != nil {
		t.Fatalf("WithPinnedBatch: %v", err)
	}

	if _, err := retained.ReadDir("."); err == nil || !strings.Contains(err.Error(), "pinned delivery batch expired") {
		t.Fatalf("retained batch ReadDir error = %v, want expired batch refusal", err)
	}
	if err := root.VerifyBase(); err != nil {
		t.Fatalf("owning root unusable after batch expiry: %v", err)
	}
}

func TestDeliveryRootPinnedBatchChildExpiresAfterCallback(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "collab"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := openDeliveryRootForTest(t, base)
	var retainedChild *DeliveryRoot

	if err := root.WithPinnedBatch(func(batch *DeliveryRoot) error {
		child, err := batch.OpenDirectChild("collab")
		if err != nil {
			return err
		}
		retainedChild = child
		return child.EnsureRootDirs()
	}); err != nil {
		t.Fatalf("WithPinnedBatch child: %v", err)
	}
	defer func() { _ = retainedChild.Close() }()

	if _, err := retainedChild.ReadDir("."); err == nil || !strings.Contains(err.Error(), "pinned delivery batch expired") {
		t.Fatalf("retained child ReadDir error = %v, want expired batch refusal", err)
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

func TestCreateDirectChildExclusiveRaceIsLoud(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	old := beforeCreateDirectChildExclusiveForTest
	beforeCreateDirectChildExclusiveForTest = func(r *DeliveryRoot, name string) {
		if err := os.Mkdir(filepath.Join(r.Base(), name), 0o700); err != nil {
			t.Fatalf("pre-create: %v", err)
		}
	}
	t.Cleanup(func() { beforeCreateDirectChildExclusiveForTest = old })

	_, err := root.CreateDirectChildExclusive("auth", 0o700)
	if err == nil {
		t.Fatal("racing creator was opened silently")
	}
	var exists *DirectChildExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("error = %v, want DirectChildExistsError", err)
	}
}
