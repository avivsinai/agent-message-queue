package fsq

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryRootOpenRegularNoFollow(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "message.md"), []byte("header only"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)
	file, info, err := root.OpenRegularNoFollow("message.md")
	if err != nil {
		t.Fatalf("OpenRegularNoFollow: %v", err)
	}
	defer func() { _ = file.Close() }()
	if !info.Mode().IsRegular() {
		t.Fatalf("mode = %v, want regular", info.Mode())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "header only" {
		t.Fatalf("data = %q", data)
	}
}

func TestDeliveryRootOpenLockFileStableInode(t *testing.T) {
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
