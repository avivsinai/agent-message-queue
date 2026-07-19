//go:build unix

package registry

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock on the open lock file,
// blocking until it is available.
func flockExclusive(lock *os.File) error {
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
}

// flockRelease releases the advisory lock taken by flockExclusive.
func flockRelease(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
