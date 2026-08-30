//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen       = unix.PidfdOpen
	linuxPidfdSendSignal = unix.PidfdSendSignal
	linuxPidfdClose      = unix.Close
	linuxPidfdPoll       = pollLinuxPidfd
)

// readWakeLockMetadata reads one exact lock generation without consulting the
// process table. Linux orphan retirement uses this to acquire a pidfd before
// the first PID-based identity inspection of the locked generation.
func readWakeLockMetadata(root, me string) wakeLockInspection {
	lockPath := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	return readWakeLockMetadataWithReader(root, me, lockPath, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileWithInfo(lockPath)
	})
}

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockWithRawConsent(inspection, false)
}

func terminateAndRemoveOrphanedWakeLockWithRawConsent(
	inspection wakeLockInspection,
	allowRawOrphan bool,
) (bool, error) {
	agentDir, err := openExistingCoopWakeAgentDir(inspection.Root, inspection.Agent)
	if err != nil {
		return false, err
	}
	if agentDir == nil {
		return false, fmt.Errorf("wake agent directory disappeared before termination")
	}
	defer func() { _ = agentDir.Close() }()
	return terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
		agentDir,
		inspection,
		allowRawOrphan,
		nil,
	)
}

func terminateAndRemoveOrphanedWakeLockInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(agentDir, inspection, false, nil)
}

func terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	allowRawOrphan bool,
	requestedTarget *wakeTarget,
) (bool, error) {
	var locked wakeLockInspection
	pidfd := -1
	provenGone := false
	if err := withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		locked = readWakeLockMetadataAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if !sameWakeLockGeneration(inspection, locked) {
			return nil
		}
		if err := validateWakeLockOwnerlessMutationAtForTermination(dirfd, agentDir, locked); err != nil {
			return err
		}
		if requestedTarget != nil {
			if err := requireExistingWakeTargetMatchesAt(dirfd, agentDir, locked, *requestedTarget); err != nil {
				return err
			}
		}
		fd, err := linuxPidfdOpen(locked.PID, 0)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				locked.Process = wakeProcessInfo{PID: locked.PID}
				classifyWakeLock(locked.Root, locked.Agent, &locked)
				var removeErr error
				outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
					dirfd,
					agentDir,
					locked,
					func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
				)
				provenGone, removeErr = outcome.Committed, outcome.Err
				return removeErr
			}
			return fmt.Errorf("pidfd_open wake process %d: %w", locked.PID, err)
		}
		pidfd = fd
		locked.Process = inspectWakeProcess(locked.PID)
		classifyWakeLock(locked.Root, locked.Agent, &locked)
		if !sameWakeLockInspection(inspection, locked) || !locked.IdentityConfirmed {
			return nil
		}
		return nil
	}); err != nil {
		if pidfd >= 0 {
			_ = linuxPidfdClose(pidfd)
		}
		return provenGone, err
	}
	if provenGone {
		return true, nil
	}
	if pidfd < 0 || !locked.IdentityConfirmed {
		if pidfd >= 0 {
			_ = linuxPidfdClose(pidfd)
		}
		return false, nil
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()

	if locked.Process.Running && locked.Lock.WakeMode != wakeTargetInjectVia &&
		!wakeLockNeedsReplacement(locked) && !allowRawOrphan {
		return false, fmt.Errorf(
			"live raw wake for %s (pid %d, start %s) is not eligible for automatic replacement; retry through amq coop exec to review and consent to takeover, or stop it from its owning session; refusing to signal without consent",
			locked.Agent,
			locked.PID,
			locked.Lock.ProcessStart,
		)
	}
	if allowRawOrphan && locked.Process.Running && locked.Lock.WakeMode != wakeTargetInjectVia &&
		!isLiveRawOrphan(locked) {
		return false, fmt.Errorf("live raw wake identity changed before consented termination")
	}
	if err := terminateWakePidfd(pidfd); err != nil {
		return resolveMissingWakeLockAfterTerminationInDir(agentDir, locked, err)
	}

	removed := false
	err := withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if !current.Exists {
			removed = true
			return nil
		}
		if !sameWakeLockGenerationForRetainedTermination(locked, current) {
			return newWakeLockResidueError(
				wakeLockResidueReplacement,
				errors.New("wake lock changed after wake process stopped; preserving replacement claim"),
			)
		}
		if retainedWakeAgentDirIsDetached(agentDir) {
			var removeErr error
			outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
				dirfd,
				agentDir,
				current,
				func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
			)
			removed, removeErr = outcome.Committed, outcome.Err
			return removeErr
		}
		if requestedTarget != nil {
			if err := requireExistingWakeTargetMatchesAt(dirfd, agentDir, current, *requestedTarget); err != nil {
				return err
			}
		}
		if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, current); err != nil {
			return newWakeLockResidueError(
				wakeLockResiduePreservedClaim,
				fmt.Errorf("wake process stopped but exact wake lock cleanup is unavailable; preserving retained claim: %w", err),
			)
		}
		var removeErr error
		outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
			dirfd,
			agentDir,
			current,
			func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
		)
		removed, removeErr = outcome.Committed, outcome.Err
		return removeErr
	})
	if err != nil {
		if len(wakeLockRemovalResiduesFromError(err)) == 0 {
			err = newWakeLockResidueError(
				wakeLockResiduePreservedClaim,
				fmt.Errorf("wake process stopped but wake lock cleanup did not complete; preserving exact claim: %w", err),
			)
		}
		return true, err
	}
	if !removed {
		return true, newWakeLockResidueError(
			wakeLockResidueCleanup,
			errors.New("wake process stopped but exact wake lock cleanup outcome changed"),
		)
	}
	return true, nil
}

func terminateWakePidfd(pidfd int) error {
	if err := linuxPidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_send_signal SIGTERM: %w", err)
	}
	exited, err := linuxPidfdPoll(pidfd, wakeTerminateGrace)
	if err != nil {
		return fmt.Errorf("poll pidfd after SIGTERM: %w", err)
	}
	if exited {
		return nil
	}
	if err := linuxPidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_send_signal SIGKILL: %w", err)
	}
	exited, err = linuxPidfdPoll(pidfd, wakeTerminateKillConfirm)
	if err != nil {
		return fmt.Errorf("poll pidfd after SIGKILL: %w", err)
	}
	if !exited {
		return fmt.Errorf("wake process still alive after SIGKILL")
	}
	return nil
}

func pollLinuxPidfd(pidfd int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		timeoutMillis := int((remaining + time.Millisecond - 1) / time.Millisecond)
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, timeoutMillis)
		if errors.Is(err, syscall.EINTR) {
			if time.Now().Before(deadline) {
				continue
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if ready == 0 {
			return false, nil
		}
		revents := fds[0].Revents
		if revents&unix.POLLNVAL != 0 {
			return false, fmt.Errorf("pidfd became invalid")
		}
		if revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return true, nil
		}
		if revents&unix.POLLERR != 0 {
			return false, fmt.Errorf("pidfd poll reported an error")
		}
	}
}
