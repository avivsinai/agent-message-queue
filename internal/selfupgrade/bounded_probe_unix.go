//go:build darwin || linux

package selfupgrade

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureBoundedProbe(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return cleanupBoundedProbeProcessGroup(command.Process.Pid)
	}
}

// A descendant that calls setsid can escape this process group; that remains
// an explicit limitation of bounded probe cleanup.
func cleanupBoundedProbeProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil &&
		!errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
