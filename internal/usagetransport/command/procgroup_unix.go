//go:build unix

package command

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// groups reports whether the child can run in a process group the runner
// kills as a whole.
const groups = true

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

// killGroup kills what is left of the child's process group after the
// child was reaped. A process group id stays reserved while any member
// lives, so the signal reaches only descendants still in the child's group;
// an empty group answers ESRCH, which is success. Descendants are not the
// runner's children: their parent of last resort reaps them.
func killGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
