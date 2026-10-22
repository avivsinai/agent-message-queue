//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// AdvisoryLockAvailable reports whether this platform has a real
// interprocess file lock.
func AdvisoryLockAvailable() bool { return true }

// Lock takes an exclusive flock on f, waiting until it is free. The lock
// belongs to the open file description: another open of the same file,
// in this process or another, excludes it.
func Lock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// TryLock takes the lock of Lock without waiting. ok is false, with no error,
// when another holder has it.
func TryLock(f *os.File) (ok bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}

// Unlock releases a lock taken by Lock or TryLock. Closing f releases it too.
func Unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
