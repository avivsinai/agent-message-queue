//go:build windows

package main

import "os/exec"

// setProcessGroup is a no-op on Windows, where amq-acp host refuses to run
// for lack of advisory file locks.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the agent process. amq-acp host refuses to start
// on Windows (no advisory file locks), so this path never runs there.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
