//go:build darwin || linux || freebsd

package launch

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockExclusive(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire launch lease lock: %w", err)
	}
	return nil
}

func unlockExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(unix.Signal(0))
	if err == nil || errors.Is(err, unix.EPERM) {
		return true
	}
	return false
}
