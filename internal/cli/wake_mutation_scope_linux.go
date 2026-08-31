//go:build linux

package cli

import (
	"errors"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/cli/wakemutation"
	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen  = unix.PidfdOpen
	linuxPidfdSend  wakemutation.PidfdSenderFunc
	linuxPidfdClose = unix.Close

	// Tests use this barrier to force a successor publication between the
	// missing-guard probe and acquisition of the permanent guard.
	wakeMutationAfterLifecycleGuardProbe = func(*wakeAgentDir, bool) {}
)

// detachedWakeCleanupScope intentionally carries only the retained directory
// capability needed to unlink residue. It cannot signal or perform any other
// lifecycle effect.
type detachedWakeCleanupScope struct {
	agentDir     *wakeAgentDir
	dirfd        int
	active       func() bool
	unlink       func() error
	releaseLease func() error
}

func newDetachedWakeCleanupScope(agentDir *wakeAgentDir, dirfd int) *detachedWakeCleanupScope {
	lease := wakemutation.New(nil)
	return &detachedWakeCleanupScope{
		agentDir: agentDir,
		dirfd:    dirfd,
		active:   lease.Active,
		unlink: func() error {
			return lease.UnlinkAt(dirfd, wakeLockFileName, 0)
		},
		releaseLease: lease.Close,
	}
}

func (scope *detachedWakeCleanupScope) release() error {
	if scope == nil || scope.releaseLease == nil {
		return unix.EINVAL
	}
	return scope.releaseLease()
}

func (scope *detachedWakeCleanupScope) location() (int, *wakeAgentDir, error) {
	if scope == nil || scope.active == nil || !scope.active() {
		return 0, nil, fmt.Errorf("detached wake cleanup scope is inactive")
	}
	if scope.agentDir == nil || scope.agentDir.file == nil {
		return 0, nil, unix.EINVAL
	}
	if scope.dirfd != int(scope.agentDir.file.Fd()) {
		return 0, nil, fmt.Errorf("detached wake cleanup scope directory capability does not match retained descriptor")
	}
	return scope.dirfd, scope.agentDir, nil
}

func (scope *detachedWakeCleanupScope) unlinkWakeLockForCleanup() error {
	if _, _, err := scope.location(); err != nil {
		return err
	}
	if scope.unlink == nil {
		return unix.EINVAL
	}
	return scope.unlink()
}

func withWakeMutationScopeRetainedDirNoGuard(
	agentDir *wakeAgentDir,
	fn func(wakeRetainedCleanupScope) error,
) error {
	return agentDir.withFD(func(dirfd int) (retErr error) {
		relation, err := retainedWakeAgentDirRelationAt(agentDir, dirfd)
		if err != nil {
			return fmt.Errorf("determine retained wake agent directory relation before no-guard cleanup: %w", err)
		}
		if relation != wakeAgentDirDetached {
			return fmt.Errorf("no-guard wake mutation requires a proven detached retained directory")
		}
		scope := newDetachedWakeCleanupScope(agentDir, dirfd)
		retErr = fn(scope)
		return errors.Join(retErr, scope.release())
	})
}

func withWakeMutationScopeForRetainedCleanup(
	agentDir *wakeAgentDir,
	allowMissing bool,
	fn func(wakeRetainedCleanupScope) error,
) error {
	if !allowMissing {
		return withExistingWakeMutationScopeNoWaitInDir(agentDir, func(scope *wakeMutationScope) error {
			return fn(scope)
		})
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
	return withWakeMutationScopeOrRetainedDirNoWait(agentDir, allowMissing, func(scope *wakeMutationScope) error {
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
