package core

import (
	"os"
	"path/filepath"
	"testing"
)

// A working directory spelled in another letter case than the state
// directory's parent is the same directory on a case-insensitive file
// system, so it counts as holding the state directory (review of the session
// grant branch: the string comparison missed it).
func TestRootInAnotherCaseHoldsTheStateDirectory(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "Home", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "home")
	if _, err := os.Stat(root); err != nil {
		t.Skip("the file system distinguishes letter case")
	}
	if !PathsOverlap(root, state) {
		t.Fatalf("PathsOverlap(%s, %s) = false; want true, the same directory", root, state)
	}
}

// A state directory reached through a symbolic link in a middle component
// (the AMQ home linked into the working directory) lies inside the working
// directory, so it counts as held (review of the session grant branch: the
// comparison by file identity alone missed it).
func TestStateLinkedIntoTheWorkingDirectoryIsHeld(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, home := filepath.Join(base, "work"), filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(work, "amq", "remote", "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "amq"), filepath.Join(home, ".amq")); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	state := filepath.Join(home, ".amq", "remote", "root")
	if !PathsOverlap(work, state) {
		t.Fatalf("PathsOverlap(%s, %s) = false; want true, the state lies inside the working directory", work, state)
	}
}
