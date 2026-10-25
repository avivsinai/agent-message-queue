//go:build unix

package command

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ownGroup starts the command in its own process group and makes
// cancellation kill the whole group, so descendants die with the command.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
