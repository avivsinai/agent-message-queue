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

func TestPublishInitializedDirectChildExclusiveNeverReplacesRacingEmptyChild(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	old := beforePublishInitializedDirectChildForTest
	beforePublishInitializedDirectChildForTest = func(r *DeliveryRoot, name string) {
		if err := os.Mkdir(filepath.Join(r.Base(), name), 0o711); err != nil {
			t.Fatalf("create racing child: %v", err)
		}
	}
	t.Cleanup(func() { beforePublishInitializedDirectChildForTest = old })

	_, err := root.PublishInitializedDirectChildExclusive("collab", 0o700, func(child *DeliveryRoot) error {
		if err := child.EnsureRootDirs(); err != nil {
			return err
		}
		return child.EnsureAgentDirs("operator")
	})
	var exists *DirectChildExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("publication error = %v, want DirectChildExistsError", err)
	}
	info, statErr := os.Stat(filepath.Join(base, "collab"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("racing child mode = %04o, want untouched 0711", info.Mode().Perm())
	}
	entries, readErr := os.ReadDir(filepath.Join(base, "collab"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("racing child was merged or replaced: %v", entries)
	}
}

func TestPublishInitializedDirectChildExclusiveReportsCommittedDurabilityFailure(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	failure := errors.New("parent sync failed")
	child, err := root.PublishInitializedDirectChildExclusive("collab", 0o700, func(child *DeliveryRoot) error {
		if err := child.EnsureRootDirs(); err != nil {
			return err
		}
		if err := child.EnsureAgentDirs("operator"); err != nil {
			return err
		}
		// The staged child captured the prior nil hook. Only the final parent
		// directory sync observes this injected failure.
		root.syncDirForTest = func(string) error { return failure }
		return nil
	})
	if child == nil {
		t.Fatal("committed publication did not return its pinned child")
	}
	defer func() { _ = child.Close() }()
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, failure) {
		t.Fatalf("publication error = %v, want committed durability failure", err)
	}
	if child.Base() != filepath.Join(base, "collab") {
		t.Fatalf("committed child base = %q", child.Base())
	}
	if err := child.VerifyBase(); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range requiredMailboxLeaves {
		if info, err := child.Stat(filepath.Join("agents", "operator", string(leaf))); err != nil || !info.IsDir() {
			t.Fatalf("committed mailbox %s: info=%v err=%v", leaf, info, err)
		}
	}
}

func TestPublishInitializedDirectChildExclusiveReportsPostRenameIdentityLossAsCommitted(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	old := afterPublishInitializedDirectChildForTest
	afterPublishInitializedDirectChildForTest = func(r *DeliveryRoot, name string) {
		if err := os.Rename(filepath.Join(r.Base(), name), filepath.Join(r.Base(), "moved-away")); err != nil {
			t.Fatalf("move published child: %v", err)
		}
	}
	t.Cleanup(func() { afterPublishInitializedDirectChildForTest = old })

	child, err := root.PublishInitializedDirectChildExclusive("collab", 0o700, func(child *DeliveryRoot) error {
		return child.EnsureRootDirs()
	})
	if child == nil {
		t.Fatal("post-rename identity loss did not return the committed capability")
	}
	defer func() { _ = child.Close() }()
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("post-rename identity error = %v, want committed error", err)
	}
	if _, err := os.Stat(filepath.Join(base, "moved-away", "agents")); err != nil {
		t.Fatalf("published tree was not retained at racing location: %v", err)
	}
}
