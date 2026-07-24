//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package registry

import (
	"errors"
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock on the open lock file,
// blocking until it is available.
func flockExclusive(lock *os.File) error {
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
}

// flockTryExclusive attempts the same lock without blocking. A busy lock is a
// normal false result so context-aware callers can poll and honor cancellation.
func flockTryExclusive(lock *os.File) (bool, error) {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func flockRelease(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
