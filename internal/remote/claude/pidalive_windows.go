//go:build windows

package claude

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// pidAlive reports whether the process is still running, without
// signalling it: OpenProcess + GetExitCodeProcess. STILL_ACTIVE means
// alive. OpenProcess on a dead pid fails with ERROR_FILE_NOT_FOUND or —
// on some Windows versions — ERROR_INVALID_PARAMETER; both mean the
// process is gone (r2 P2-3). Any other open failure is an error.
func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	const processQueryLimitedInformation = windows.PROCESS_QUERY_LIMITED_INFORMATION
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		return false, fmt.Errorf("openprocess(%d): %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, fmt.Errorf("getexitcodeprocess(%d): %w", pid, err)
	}
	const stillActive = 259
	return code == stillActive, nil
}
