//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package notificationattempt

import (
	"os"
	"syscall"
)

// flockShared takes a shared advisory lock on the open lock file, blocking
// until it is available. Shared locks do not contend with each other, so
// concurrent appenders stay concurrent — the O_APPEND hot path keeps its
// lock-free-among-appenders property. The lock exists only to exclude the
// rare rotation path, which takes LOCK_EX.
//
// The lock file is distinct from the data file (see append), so locking it
// does not race with rotation truncating/recreating the data file — flock is
// per-inode and the lock file's inode never changes.
func flockShared(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
}

// flockExclusive takes an exclusive advisory lock on the open lock file,
// blocking until all shared (append) and exclusive (rotation) holders have
// released. This serializes rotation against the hot path and against other
// rotators, closing the read→merge→truncate window where an O_APPEND writer
// could otherwise land a record that the subsequent truncate discards.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockRelease releases any held advisory lock. Safe to call on an unlocked fd.
// A single LOCK_SH call after holding LOCK_EX atomically downgrades EX→SH,
// letting waiting appenders proceed while we keep SH for our own append.
func flockRelease(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
