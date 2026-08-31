//go:build darwin || linux

package wakemutation

import (
	"os"
	"syscall"
)

func processSignalAlive(process *os.Process) bool {
	err := process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
