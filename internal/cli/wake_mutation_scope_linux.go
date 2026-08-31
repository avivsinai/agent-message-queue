//go:build linux

package cli

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen       = unix.PidfdOpen
	linuxPidfdSendSignal = sendLinuxPidfdSignalRaw
	linuxPidfdClose      = unix.Close
)

func withWakeMutationScopeRetainedDirNoGuard(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return agentDir.withFD(func(dirfd int) (retErr error) {
		scope := newWakeMutationScope(agentDir, dirfd, nil)
		defer func() { retErr = errors.Join(retErr, scope.release()) }()
		return fn(scope)
	})
}

func withWakeMutationScopeOrRetainedDirNoWait(
	agentDir *wakeAgentDir,
	allowMissing bool,
	fn func(*wakeMutationScope) error,
) error {
	if !allowMissing {
		return withExistingWakeMutationScopeNoWaitInDir(agentDir, fn)
	}
	missing, err := wakeLifecycleGuardMissingAt(agentDir)
	if err != nil {
		return err
	}
	if missing {
		return withWakeMutationScopeNoWaitInDir(agentDir, fn)
	}
	return withExistingWakeMutationScopeNoWaitInDir(agentDir, fn)
}

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
func (scope *wakeMutationScope) sendPidfdSignalForTermination(pidfd int, signal unix.Signal) (bool, error) {
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return false, err
	}
	return true, sendWakePidfdSignal(pidfd, signal)
}

func sendWakePidfdSignal(pidfd int, signal unix.Signal) error {
	if err := linuxPidfdSendSignal(pidfd, signal, nil, 0); err != nil {
		return fmt.Errorf("pidfd_send_signal %s: %w", wakeSignalName(signal), err)
	}
	return nil
}

func wakeSignalName(signal unix.Signal) string {
	switch signal {
	case unix.SIGTERM:
		return "SIGTERM"
	case unix.SIGKILL:
		return "SIGKILL"
	default:
		return fmt.Sprintf("signal %d", signal)
	}
}
