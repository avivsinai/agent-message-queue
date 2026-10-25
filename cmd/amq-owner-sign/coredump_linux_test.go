//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Codex review of #1211: a core_pattern that pipes to systemd-coredump or
// Apport ignores RLIMIT_CORE, so on Linux the signer must also be undumpable.
func checkNoPipedDump(t *testing.T) {
	t.Helper()
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dumpable != 0 {
		t.Fatalf("PR_GET_DUMPABLE after start = %d, want 0", dumpable)
	}
}
