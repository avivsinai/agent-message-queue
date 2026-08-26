//go:build unix

package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

type platformProcessSpawner struct{}

func (platformProcessSpawner) Start(_ context.Context, spec processSpec) (startedProcess, error) {
	log, err := os.OpenFile(spec.LogPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	_ = log.Close()
	return &unixStartedProcess{cmd: cmd}, nil
}

type unixStartedProcess struct {
	cmd *exec.Cmd
}

func (p *unixStartedProcess) PID() int { return p.cmd.Process.Pid }

func (p *unixStartedProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *unixStartedProcess) KillGroup() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
	if err != nil && err != unix.ESRCH {
		_ = p.cmd.Process.Kill()
		return err
	}
	return nil
}

func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := unix.Kill(pid, 0)
	if err == nil || err == unix.EPERM {
		return true, nil
	}
	if err == unix.ESRCH {
		return false, nil
	}
	return false, fmt.Errorf("kill(pid,0): %w", err)
}
