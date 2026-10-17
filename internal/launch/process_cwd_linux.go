//go:build linux

package launch

import (
	"fmt"
	"os"
)

// ProcessCwd is the current working directory of process pid.
func ProcessCwd(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	return os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
}
