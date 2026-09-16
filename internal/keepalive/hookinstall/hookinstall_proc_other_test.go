//go:build !darwin && !linux

package hookinstall

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on non-Unix platforms.
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup kills the process on non-Unix platforms. Windows does
// not have process groups; cmd.Cancel must kill the process directly since
// assigning cmd.Cancel replaces exec.CommandContext's default Process.Kill.
func killProcessGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
