//go:build unix

package hookinstall

import "syscall"

func processRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func killPID(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
