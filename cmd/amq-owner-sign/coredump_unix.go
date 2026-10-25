//go:build unix

package main

import "golang.org/x/sys/unix"

// forbidCoreDumps sets the soft and hard core size limits to zero, so no
// crash of this process, whatever GOTRACEBACK says, writes its memory and
// the owner key to a core file. Lowering a limit needs no privilege, and a
// zero hard limit cannot be raised again by this process.
func forbidCoreDumps() error {
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
}
