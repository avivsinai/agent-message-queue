//go:build !windows

package claude

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// pidAlive reports whether the process is still running, without
// signalling it: kill(pid, 0) performs the permission check only. EPERM
// still means alive (the process exists, we just may not signal it).
func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := unix.Kill(pid, 0)
	if err == nil || err == unix.EPERM {
		return true, nil
	}
	if err == unix.ESRCH {
		return false, nil
	}
	return false, fmt.Errorf("kill(pid,0): %w", err)
}
