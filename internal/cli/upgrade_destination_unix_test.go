//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeDestinationWritableRejectsReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses Unix permission checks")
	}
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("manual"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatalf("chmod binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })

	err := upgradeDestinationWritable(path)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("upgradeDestinationWritable error = %v, want permission error", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Fatalf("upgradeDestinationWritable path error = %#v, want path %q", pathErr, path)
	}
}

func TestUpgradeDestinationWritableRejectsReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses Unix permission checks")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir binary directory: %v", err)
	}
	path := filepath.Join(dir, "amq")
	if err := os.WriteFile(path, []byte("manual"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod binary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := upgradeDestinationWritable(path)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("upgradeDestinationWritable error = %v, want permission error", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != dir {
		t.Fatalf("upgradeDestinationWritable path error = %#v, want directory %q", pathErr, dir)
	}
}
