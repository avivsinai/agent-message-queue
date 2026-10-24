//go:build darwin

package selfupgrade

import (
	"os"
	"path/filepath"
	"testing"
)

// vd4.8: concurrent wakes and brew cleanup link and unlink stage names for
// the same installed inode during a hash. The ctime change killed the
// upgraded wake after exec and left a stale lock.
func TestCaptureImageEvidenceAcceptsSiblingHardLinkDuringHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "amq")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "amq-stage")
	_, err := CaptureImageEvidenceWithMutator(path, "1.2.3", func() {
		if linkErr := os.Link(path, sibling); linkErr != nil {
			t.Errorf("link sibling: %v", linkErr)
		}
		if removeErr := os.Remove(sibling); removeErr != nil {
			t.Errorf("remove sibling: %v", removeErr)
		}
	})
	if err != nil {
		t.Fatalf("CaptureImageEvidenceWithMutator: %v", err)
	}
}
