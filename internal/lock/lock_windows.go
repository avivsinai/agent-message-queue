//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// AdvisoryLockAvailable reports whether this platform has a real
// interprocess file lock.
func AdvisoryLockAvailable() bool { return true }

// Lock takes an exclusive LockFileEx lock on f, waiting until it is free.
// The lock belongs to the handle: another open of the same file, in this
// process or another, excludes it. Every holder locks the same one byte at
// offset 0, so any lock that covers that byte excludes it too.
func Lock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped)
}

// TryLock takes the lock of Lock without waiting. ok is false, with no error,
// when another holder has it.
func TryLock(f *os.File) (ok bool, err error) {
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// Unlock releases a lock taken by Lock or TryLock. Closing f releases it too.
func Unlock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
