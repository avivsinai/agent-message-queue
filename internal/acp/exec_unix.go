//go:build !windows

package acp

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// canExecute reports execute access for this process's effective uid/gid
// (AT_EACCESS), not the real ones that plain access(2) checks. A test cannot
// split real and effective ids without privileges, so none covers this.
func canExecute(path string) bool {
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}

// ownProcessGroup runs cmd in its own process group and, when its context
// ends, kills the whole group, so a child that inherited its pipes dies too.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
