//go:build darwin || linux || freebsd

package launch

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

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
