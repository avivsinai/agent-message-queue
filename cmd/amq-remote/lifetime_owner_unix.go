//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"syscall"
)

// lifetimeOwnerPidOS reports the pid holding the entry's lifetime lock via
// fcntl F_GETLK (N3, review-b5: the exit-6 refusal must name the owner).
// BSD flock does not record a pid, so the honest answer is often "unknown"
// (0): F_GETLK checks POSIX byte-range locks, and a pure flock(2) holder is
// not reported as one. We take BOTH on the lock file (flockLifetime holds
// flock; the registry's own lock file historically may hold neither), so
// this is best-effort discovery, never an error source: the refusal goes
// out with whatever the kernel can tell us, 0 otherwise.
func lifetimeOwnerPidOS(regPath, entryID string) int {
	lockPath := lockFilePath(regPath, entryID)
	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0o600)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	var fl syscall.Flock_t
	fl.Type = syscall.F_WRLCK
	fl.Whence = 0 // SEEK_SET
	fl.Start = 0
	fl.Len = 0 // whole file
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &fl); err != nil {
		return 0
	}
	if fl.Type == syscall.F_UNLCK {
		// No POSIX lock holder discoverable (pure flock(2) holder or no
		// holder at all): report unknown rather than guess.
		return 0
	}
	if fl.Pid > 0 {
		return int(fl.Pid)
	}
	return 0
}
