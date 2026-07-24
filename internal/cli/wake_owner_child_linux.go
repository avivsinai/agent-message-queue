//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func prepareAuthoritativeWakeChildPlatform(cmd *exec.Cmd) (*authoritativeWakeChildCapability, error) {
	// Prove the syscall boundary before launch so a conclusively unsupported
	// host can choose the fresh ownerless path before an owner child exists.
	probe, err := linuxPidfdOpen(os.Getpid(), 0)
	if err != nil {
		if wakeOwnerCapabilityUnsupported(err) {
			return nil, &wakeOwnerChildCapabilityUnsupportedError{
				Err: fmt.Errorf("preflight owner wake child pidfd: %w", err),
			}
		}
		return nil, fmt.Errorf("preflight owner wake child pidfd: %w", err)
	}
	if err := linuxPidfdClose(probe); err != nil {
		return nil, fmt.Errorf("close owner wake child pidfd preflight: %w", err)
	}
	if cmd == nil {
		return nil, fmt.Errorf("authoritative wake child command is missing")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.PidFD != nil {
		return nil, fmt.Errorf("authoritative wake child command already owns a pidfd request")
	}

	// Ask os/exec to return the pidfd atomically with child creation. Opening
	// one by numeric PID after Start would leave a PID-reuse window if the
	// short-lived helper exited before the open.
	pidfd := -1
	cmd.SysProcAttr.PidFD = &pidfd
	return &authoritativeWakeChildCapability{
		bind: func(process *os.Process) error {
			if process == nil || process.Pid <= 0 {
				return fmt.Errorf("authoritative wake child process is missing")
			}
			if pidfd < 0 {
				return fmt.Errorf("authoritative wake child pidfd was not returned at process creation")
			}
			return nil
		},
		stop: func() error {
			if pidfd < 0 {
				return fmt.Errorf("authoritative wake child pidfd is unavailable")
			}
			return terminateWakePidfd(pidfd)
		},
		close: func() error {
			if pidfd < 0 {
				return nil
			}
			fd := pidfd
			pidfd = -1
			return linuxPidfdClose(fd)
		},
	}, nil
}

func authoritativeWakePrivateStopFromEnv() (<-chan struct{}, func(), error) {
	return nil, func() {}, nil
}
