//go:build windows

package fsq

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// tryExclusiveFileLock holds an exclusive LockFileEx lock on file for the
// duration of fn without waiting. It returns ErrLockBusy when another holder
// has the lock.
func tryExclusiveFileLock(file *os.File, fn func() error) error {
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return ErrLockBusy
		}
		return fmt.Errorf("acquire file lock: %w", err)
	}
	defer func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped) }()
	return fn()
}

func withExclusiveDLQEnvelopeLock(file *os.File, fn func() error) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("acquire DLQ envelope lock: %w", err)
	}
	defer func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped) }()
	return fn()
}

// WithExclusiveFileLock holds an exclusive LockFileEx lock on file for the
// duration of fn. The file must have been created via OpenLockFile (stable
// name, never replaced). The lock is kernel-released on close or process
// crash; there is no stale sentinel to clean up.
func WithExclusiveFileLock(file *os.File, fn func() error) error {
	return withExclusiveDLQEnvelopeLock(file, fn)
}
