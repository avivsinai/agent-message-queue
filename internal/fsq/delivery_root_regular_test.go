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

func TestWriteFileExclusiveDoesNotReplace(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatal(err)
	}
	root := openDeliveryRootForTest(t, base)
	dir := filepath.Join("agents", "alice", "receipts")
	if _, err := root.WriteFileExclusive(dir, "once.json", []byte("first\n"), 0o600); err != nil {
		t.Fatalf("first WriteFileExclusive: %v", err)
	}
	if _, err := root.WriteFileExclusive(dir, "once.json", []byte("second\n"), 0o600); err == nil {
		t.Fatal("second WriteFileExclusive replaced an existing name")
	}
	data, err := os.ReadFile(filepath.Join(base, dir, "once.json"))
	if err != nil || string(data) != "first\n" {
		t.Fatalf("exclusive file contents = %q, %v", data, err)
	}
}
