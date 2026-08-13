//go:build !darwin && !linux && !freebsd && !windows

package launch

import (
	"fmt"
	"os"
)

func lockExclusive(*os.File) error {
	return fmt.Errorf("launch lease locking is unsupported on this platform")
}

func unlockExclusive(*os.File) error { return nil }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
