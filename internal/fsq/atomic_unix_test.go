//go:build !windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Review of the state-file writer: a symlinked directory below the trusted
// root redirected the write outside it.
func TestWriteStateFileRefusesSymlinkedDirectoryBelowRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	_, err := WriteStateFile(root, filepath.Join("link", "state.json"), []byte("x"), 0o600)
	if !errors.Is(err, ErrPathIsSymlink) {
		t.Fatalf("WriteStateFile through a symlinked directory: err = %v, want ErrPathIsSymlink", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("write landed outside the root: %v", entries)
	}
}
