package fsq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restore for the 2026-09-06 cull regression gate: one test per lost
// fail-closed or user-visible contract.

func TestDeliveryRootOpenLockFileKeepsOneInode(t *testing.T) {
	base := t.TempDir()
	root := openDeliveryRootForTest(t, base)
	first, err := root.OpenLockFile("meta/launch", "lease.lock", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := root.OpenLockFile("meta/launch", "lease.lock", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	a, err := first.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("OpenLockFile replaced the lock inode")
	}
}

func TestClassifyLayoutRejectsAgentsSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "agents")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	root := openDeliveryRootForTest(t, base)
	state, err := root.ClassifyLayout()
	if err == nil || state == LayoutInitialized {
		t.Fatalf("ClassifyLayout = %v, %v; want foreign and error", state, err)
	}
}

func TestOpenDirectChildPinsExistingDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "collab"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := openDeliveryRootForTest(t, base)
	child, err := root.OpenDirectChild("collab")
	if err != nil {
		t.Fatalf("OpenDirectChild: %v", err)
	}
	defer func() { _ = child.Close() }()
	if _, err := child.Stat("."); err != nil {
		t.Fatalf("opened child unusable: %v", err)
	}
	if _, err := root.OpenDirectChild("no-such-child"); err == nil {
		t.Fatal("OpenDirectChild invented a missing child")
	}
}

func TestCreateExclusiveFileRefusesReplacement(t *testing.T) {
	base := t.TempDir()
	if err := EnsureRootDirs(base); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	root := openDeliveryRootForTest(t, base)
	path := "agents/cursor/outbox/acp-events/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"
	if err := root.CreateExclusiveFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("first CreateExclusiveFile: %v", err)
	}
	if err := root.CreateExclusiveFile(path, []byte("second\n"), 0o600); !os.IsExist(err) {
		t.Fatalf("replacement error = %v, want os.ErrExist", err)
	}
	got, err := root.ReadRegularNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" {
		t.Fatalf("file = %q, want the first write", got)
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
