//go:build unix

package main

import (
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

// Issue #1211: a crash between the key read and the policy publication must
// not write the owner key to a core file, so the signer forbids core dumps
// before it can read the key.
func TestSignerForbidsCoreDumpsBeforeItRuns(t *testing.T) {
	runMain([]string{"--version"}, io.Discard, io.Discard)
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Cur != 0 || lim.Max != 0 {
		t.Fatalf("RLIMIT_CORE after start = %d/%d, want 0/0", lim.Cur, lim.Max)
	}
}
