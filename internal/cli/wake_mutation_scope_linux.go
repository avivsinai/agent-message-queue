//go:build linux

package cli

import (
	"errors"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/cli/wakemutation"
	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen                               = unix.PidfdOpen
	linuxPidfdSend  wakemutation.PidfdSenderFunc = wakemutation.PidfdSendSignal
	linuxPidfdClose                              = unix.Close

	// Tests use this barrier to force a successor publication between the
	// missing-guard probe and acquisition of the permanent guard.
	wakeMutationAfterLifecycleGuardProbe = func(*wakeAgentDir, bool) {}
)

func withWakeMutationScopeRetainedDirNoGuard(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return agentDir.withFD(func(dirfd int) (retErr error) {
		relation, err := retainedWakeAgentDirRelationAt(agentDir, dirfd)
		if err != nil {
			return fmt.Errorf("determine retained wake agent directory relation before no-guard cleanup: %w", err)
		}
		if relation != wakeAgentDirDetached {
			return fmt.Errorf("no-guard wake mutation requires a proven detached retained directory")
		}
		scope := newWakeMutationScope(agentDir, dirfd, nil)
		retErr = fn(scope)
		return errors.Join(retErr, scope.release())
	})
}

func withWakeMutationScopeForRetainedCleanup(
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
		relation, err := retainedWakeAgentDirRelation(agentDir)
		if err != nil {
			return err
		}
		if relation == wakeAgentDirDetached {
			return withWakeMutationScopeRetainedDirNoGuard(agentDir, fn)
		}
	}
	return withWakeMutationScopeOrRetainedDirNoWait(agentDir, allowMissing, fn)
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
	wakeMutationAfterLifecycleGuardProbe(agentDir, missing)
	if missing {
		return withWakeMutationScopeNoWaitInDir(agentDir, fn)
	}
	return withExistingWakeMutationScopeNoWaitInDir(agentDir, fn)
}

// sendPidfdSignal is for non-termination effects. It requires the retained
// directory to remain canonical before signaling.
func (scope *wakeMutationScope) sendPidfdSignal(pidfd int, signal unix.Signal) error {
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return sendWakePidfdSignalWithLease(scope.lease, pidfd, signal)
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
	return true, sendWakePidfdSignalWithLease(scope.lease, pidfd, signal)
}

func sendWakePidfdSignal(pidfd int, signal unix.Signal) error {
	lease := wakemutation.New(nil)
	defer func() { _ = lease.Close() }()
	return sendWakePidfdSignalWithLease(lease, pidfd, signal)
}

func sendWakePidfdSignalWithLease(lease *wakemutation.Lease, pidfd int, signal unix.Signal) error {
	var err error
	if linuxPidfdSend == nil {
		err = lease.SendPidfdSignal(pidfd, signal)
	} else {
		err = lease.SendPidfdSignalWith(linuxPidfdSend, pidfd, signal)
	}
	if err != nil {
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
