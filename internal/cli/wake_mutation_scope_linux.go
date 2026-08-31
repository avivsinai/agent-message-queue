//go:build linux

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen       = unix.PidfdOpen
	linuxPidfdSendSignal = sendLinuxPidfdSignalRaw
	linuxPidfdClose      = unix.Close
)

func sendLinuxPidfdSignalRaw(pidfd int, signal unix.Signal, info *unix.Siginfo, flags int) error {
	return unix.PidfdSendSignal(pidfd, signal, info, flags)
}

// sendPidfdSignal is for non-termination effects. It requires the retained
// directory to remain canonical before signaling.
func (scope *wakeMutationScope) sendPidfdSignal(pidfd int, signal unix.Signal) error {
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return sendWakePidfdSignal(pidfd, signal)
}

// sendPidfdSignalForTermination is only for intentionally ending the exact
// wake process already authorized by termination. Its pidfd pins that
// process; canonical or proven-detached retained authority is sufficient,
// while an inconclusive relation still refuses the effect. Every other signal
// uses sendPidfdSignal.
func (scope *wakeMutationScope) sendPidfdSignalForTermination(pidfd int, signal unix.Signal) error {
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	return sendWakePidfdSignal(pidfd, signal)
}

func sendWakePidfdSignal(pidfd int, signal unix.Signal) error {
	if err := linuxPidfdSendSignal(pidfd, signal, nil, 0); err != nil {
		return fmt.Errorf("pidfd_send_signal %s: %w", signal, err)
	}
	return nil
}
