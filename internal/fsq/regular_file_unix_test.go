//go:build unix

package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for bead agent-message-queue-0ed.4 (audit): the buzzio ledger
// and ACP claim readers checked the leaf with lstat and then opened it
// following symlinks, so a symlink swapped in after the check was read.
func TestReadRegularNoFollowBoundedRefusesSymlinkSwappedAfterLstat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterRegularLstatForTest = func(p string) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, p); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	t.Cleanup(func() { afterRegularLstatForTest = nil })

	if data, err := ReadRegularNoFollowBounded(path, 64); err == nil {
		t.Fatalf("read through a swapped symlink: %q", data)
	}
}

// Regression for CI run 39592519937: the request store replaces records by
// rename, and a reader that lost the race refused the new record as
// "replaced between lstat and open", so a remote result read as unknown.
func TestReadRegularNoFollowBoundedReadsAFileRenamedOverAfterLstat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterRegularLstatForTest = func(p string) {
		next := filepath.Join(dir, "next")
		if err := os.WriteFile(next, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterRegularLstatForTest = nil })

	data, err := ReadRegularNoFollowBounded(path, 64)
	if err != nil || string(data) != "new" {
		t.Fatalf("read = %q, %v; want the replacement", data, err)
	}
}
