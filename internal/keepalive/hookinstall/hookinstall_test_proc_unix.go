//go:build darwin || linux

package hookinstall

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group so that
// cmd.Cancel can kill the entire group (script + descendants, including
// the reattach job that uses set -m for its own group).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the entire process group led by pid.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
