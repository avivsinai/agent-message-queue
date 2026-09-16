//go:build !darwin && !linux

package hookinstall

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on non-Unix platforms.
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup kills the process on non-Unix platforms.
func killProcessGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// killReattachGroup kills a specific child process on non-Unix platforms.
func killReattachGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
