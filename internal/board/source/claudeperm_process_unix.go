//go:build !windows

package source

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Signal zero checks existence without sending a signal. EPERM also means
// the process exists; the board does not need permission to signal it.
func claudePermissionProcessAlive(pid int) (bool, error) {
	if pid <= 0 || int64(pid) > 1<<31-1 {
		return false, nil
	}
	err := unix.Kill(pid, 0)
	if err == nil || err == unix.EPERM {
		return true, nil
	}
	if err == unix.ESRCH {
		return false, nil
	}
	return false, fmt.Errorf("check process %d: %w", pid, err)
}
