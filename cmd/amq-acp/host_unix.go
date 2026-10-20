//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the agent in its own process group, so a terminal
// Ctrl-C reaches only the host, which then ends the turn and closes the
// agent's stdin.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the agent and everything it started.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
