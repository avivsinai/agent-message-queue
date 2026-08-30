//go:build linux

package cli

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen       = unix.PidfdOpen
	linuxPidfdSendSignal = unix.PidfdSendSignal
	linuxPidfdClose      = unix.Close
)

func (scope *wakeMutationScope) sendPidfdSignal(pidfd int, signal unix.Signal) error {
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return sendWakePidfdSignal(pidfd, signal)
}

func sendWakePidfdSignal(pidfd int, signal unix.Signal) error {
	if err := linuxPidfdSendSignal(pidfd, signal, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_send_signal %s: %w", signal, err)
	}
	return nil
}
