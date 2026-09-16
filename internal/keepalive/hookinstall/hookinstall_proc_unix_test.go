//go:build darwin || linux

package hookinstall

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group so cleanup
// can target it specifically.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the outer process group (bash + watchdog).
// This is a bounded single-syscall operation.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// killReattachGroup kills the reattach job's separate process group
// (created by set -m). The PID is fixture-owned: the script writes
// reattach_pid to a file immediately after $!, before the binary starts.
// This is a bounded single-syscall operation — no recursive scanner.
func killReattachGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
