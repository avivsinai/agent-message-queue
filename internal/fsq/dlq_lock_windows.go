//go:build windows

package fsq

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func withExclusiveDLQEnvelopeLock(file *os.File, fn func() error) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("acquire DLQ envelope lock: %w", err)
	}
	defer func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped) }()
	return fn()
}
