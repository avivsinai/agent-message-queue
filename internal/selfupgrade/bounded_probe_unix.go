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
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
