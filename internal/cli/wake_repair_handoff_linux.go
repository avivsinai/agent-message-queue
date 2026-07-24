//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

var (
	stopWakeRepairChildPidfd  = terminateWakePidfd
	closeWakeRepairChildPidfd = linuxPidfdClose
)

func prepareWakeRepairChildCapabilityPlatform(cmd *exec.Cmd) (*wakeRepairChildCapability, error) {
	if cmd == nil {
		return nil, fmt.Errorf("wake repair child command is missing")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.PidFD != nil {
		return nil, fmt.Errorf("wake repair child command already owns a pidfd request")
	}

	// os/exec fills this descriptor as part of child creation. Opening by PID
	// after Start would allow a short-lived child to exit and the PID to be
	// reused before the cleanup capability was acquired.
	pidfd := -1
	cmd.SysProcAttr.PidFD = &pidfd

	var mutex sync.Mutex
	bound := false
	detached := false
	return &wakeRepairChildCapability{
		bind: func(process *os.Process) error {
			mutex.Lock()
			defer mutex.Unlock()
			if process == nil || process.Pid <= 0 {
				return fmt.Errorf("wake repair child process is missing")
			}
			if detached {
				return fmt.Errorf("wake repair child capability is detached")
			}
			if pidfd < 0 {
				return fmt.Errorf("wake repair child pidfd was not returned at process creation")
			}
			bound = true
			return nil
		},
		stop: func() error {
			mutex.Lock()
			defer mutex.Unlock()
			if !bound || pidfd < 0 || detached {
				return fmt.Errorf("wake repair child pidfd is unavailable")
			}
			return stopWakeRepairChildPidfd(pidfd)
		},
		detach: func() error {
			mutex.Lock()
			defer mutex.Unlock()
			if !bound || pidfd < 0 || detached {
				return fmt.Errorf("wake repair child pidfd is unavailable")
			}
			fd := pidfd
			// Linux releases the descriptor early in close(2), including when
			// close later reports an error. Commit detachment before the attempt
			// and never retain, retry, or signal through the numeric fd again.
			pidfd = -1
			detached = true
			_ = closeWakeRepairChildPidfd(fd)
			return nil
		},
		close: func() error {
			mutex.Lock()
			defer mutex.Unlock()
			if pidfd < 0 {
				return nil
			}
			fd := pidfd
			pidfd = -1
			return closeWakeRepairChildPidfd(fd)
		},
	}, nil
}

func wakeRepairChildStopFromEnv() (<-chan struct{}, func(), error) {
	return nil, func() {}, nil
}
