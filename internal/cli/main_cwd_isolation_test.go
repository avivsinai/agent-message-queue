package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The test process must not run from inside a checkout that can carry a live
// queue: the cwd-local routing guard would read that queue as repo-local
// evidence and refuse every pinned temp-root operation (issue #707). Reverting
// the TestMain chdir fails this everywhere, not only on a poisoned machine.
func TestProcessWorkingDirectoryIsIsolatedFromRepoLocalQueue(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(cliSecureTempRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("test cwd = %s, want the secure temp root %s", got, want)
	}
	local, ok, err := cwdLocalMailboxRootForSession("")
	if err != nil {
		t.Fatalf("cwd-local probe: %v", err)
	}
	if ok {
		t.Fatalf("cwd-local probe found an initialized queue %s from the test cwd", local)
	}
}
