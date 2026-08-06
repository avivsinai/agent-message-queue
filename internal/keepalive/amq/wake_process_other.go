//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package amq

import "os/exec"

func configureWakeProcess(cmd *exec.Cmd) {
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
}
