//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package amq

import (
	"os/exec"
	"syscall"
)

func configureWakeProcess(cmd *exec.Cmd) {
	// A wake is a long-lived daemon-like process. Give it a separate session so
	// terminal-generated SIGINT/SIGHUP and caller shutdown do not reach it, and
	// leave stdio nil so os/exec connects it to the null device rather than
	// retaining the caller's surface PTY or launchd log descriptors.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
}
