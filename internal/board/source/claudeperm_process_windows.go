//go:build windows

package source

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func claudePermissionProcessAlive(pid int) (bool, error) {
	if pid <= 0 || uint64(pid) > 1<<32-1 {
		return false, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER || err == windows.ERROR_FILE_NOT_FOUND {
			return false, nil
		}
		return false, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, fmt.Errorf("read process %d exit code: %w", pid, err)
	}
	const stillActive = 259
	return code == stillActive, nil
}
