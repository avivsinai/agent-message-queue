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
	linuxPidfdPoll = pollLinuxPidfd
)

// readWakeLockMetadata reads one exact lock generation without consulting the
// process table. Linux orphan retirement uses this to acquire a pidfd before
// the first PID-based identity inspection of the locked generation.
func readWakeLockMetadata(root, me string) wakeLockInspection {
	lockPath := filepath.Join(fsq.AgentBase(root, me), wakeLockFileName)
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

// Legacy terminal wakes may have no lifecycle guard. Create a transient guard
// before any exact termination effect and keep it until cleanup commits.
func withExistingWakeLifecycleGuardOrRetainedDir(
	agentDir *wakeAgentDir,
	allowMissing bool,
	fn func(int) error,
) error {
	if !allowMissing {
		return withExistingWakeLifecycleGuardInDir(agentDir, fn)
	}
	missing, err := wakeLifecycleGuardMissingAt(agentDir)
	if err != nil {
		return err
	}
	if missing {
		return withWakeLifecycleGuardInDir(agentDir, fn)
	}
	return withExistingWakeLifecycleGuardInDir(agentDir, fn)
}

func wakeLockMayUseRetainedDirWithoutGuard(inspection wakeLockInspection) bool {
	if wakeLockHasOwnerMarkers(inspection) {
		return false
	}
	switch inspection.Lock.WakeMode {
	case "", wakeInjectModeRaw, wakeInjectModePaste, wakeInjectModeNone:
		return true
	default:
		return false
	}
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
	allowMissingGuard := wakeLockMayUseRetainedDirWithoutGuard(inspection)
	legacyGuardCreated := false
	if allowMissingGuard {
		missing, err := wakeLifecycleGuardMissingAt(agentDir)
		if err != nil {
			return false, err
		}
		legacyGuardCreated = missing
	}
	if err := withExistingWakeLifecycleGuardOrRetainedDir(agentDir, allowMissingGuard, func(dirfd int) error {
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
				scope := newWakeMutationScope(agentDir, dirfd)
				outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
					dirfd,
					agentDir,
					locked,
					scope.unlinkWakeLockForCleanup,
				)
				if outcome.Committed && legacyGuardCreated {
					outcome.Err = errors.Join(outcome.Err, scope.unlinkLifecycleGuard())
				}
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
	if err := terminateWakePidfdWithAuthorization(
		agentDir,
		locked,
		requestedTarget,
		allowMissingGuard,
		pidfd,
	); err != nil {
		return resolveMissingWakeLockAfterTerminationInDir(agentDir, locked, err)
	}

	removed := false
	err := withExistingWakeLifecycleGuardOrRetainedDir(agentDir, allowMissingGuard, func(dirfd int) error {
		current := inspectWakeLockAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if !current.Exists {
			removed = true
			if legacyGuardCreated {
				return newWakeMutationScope(agentDir, dirfd).unlinkLifecycleGuard()
			}
			return nil
		}
		if !sameWakeLockGenerationForRetainedTermination(locked, current) {
			return newWakeLockResidueError(
				wakeLockResidueReplacement,
				errors.New("wake lock changed after wake process stopped; preserving replacement claim"),
			)
		}
		relation, relationErr := retainedWakeAgentDirRelation(agentDir)
		if relationErr != nil {
			return newWakeLockResidueError(
				wakeLockResiduePreservedClaim,
				fmt.Errorf("wake agent directory relation is inconclusive after wake process stopped: %w", relationErr),
			)
		}
		switch relation {
		case wakeAgentDirDetached:
			var removeErr error
			scope := newWakeMutationScope(agentDir, dirfd)
			outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
				dirfd,
				agentDir,
				current,
				scope.unlinkWakeLockForCleanup,
			)
			if outcome.Committed && legacyGuardCreated {
				outcome.Err = errors.Join(outcome.Err, scope.unlinkLifecycleGuard())
			}
			removed, removeErr = outcome.Committed, outcome.Err
			return removeErr
		case wakeAgentDirCanonical:
		case wakeAgentDirInconclusive:
			return newWakeLockResidueError(
				wakeLockResiduePreservedClaim,
				fmt.Errorf("wake agent directory relation is inconclusive after wake process stopped"),
			)
		default:
			return newWakeLockResidueError(
				wakeLockResiduePreservedClaim,
				fmt.Errorf("unknown wake agent directory relation %d", relation),
			)
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
		scope := newWakeMutationScope(agentDir, dirfd)
		outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
			dirfd,
			agentDir,
			current,
			scope.unlinkWakeLockForCleanup,
		)
		if outcome.Committed && legacyGuardCreated {
			outcome.Err = errors.Join(outcome.Err, scope.unlinkLifecycleGuard())
		}
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
	send := func(signal unix.Signal) error {
		return sendWakePidfdSignal(pidfd, signal)
	}
	return terminateWakePidfdWithSignalAuthorization(pidfd, send)
}

func terminateWakePidfdWithAuthorization(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	requestedTarget *wakeTarget,
	allowMissingGuard bool,
	pidfd int,
) error {
	send := func(signal unix.Signal) error {
		return withWakeMutationScopeOrRetainedDir(
			agentDir,
			allowMissingGuard,
			unix.LOCK_EX|unix.LOCK_NB,
			func(scope *wakeMutationScope) error {
				current := inspectWakeLockAt(
					scope.dirfd,
					scope.agentDir,
					expected.Root,
					expected.Agent,
				)
				if current.Exists {
					if !sameWakeLockGeneration(expected, current) {
						return fmt.Errorf(
							"wake lock generation changed before %s; refusing to signal; inspect with `amq wake check --root %s --me %s --json`",
							signal,
							expected.Root,
							expected.Agent,
						)
					}
					if err := validateWakeLockOwnerlessMutationAtForTermination(
						scope.dirfd,
						scope.agentDir,
						current,
					); err != nil {
						return fmt.Errorf("authorize wake before %s: %w", signal, err)
					}
					if requestedTarget != nil {
						if err := requireExistingWakeTargetMatchesAt(
							scope.dirfd,
							scope.agentDir,
							current,
							*requestedTarget,
						); err != nil {
							return fmt.Errorf("authorize wake target before %s: %w", signal, err)
						}
					}
				}
				return scope.sendPidfdSignal(pidfd, signal)
			},
		)
	}
	return terminateWakePidfdWithSignalAuthorization(pidfd, send)
}

func terminateAuthoritativeWakePidfdWithAuthorization(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	auth wakeOwnerReleaseAuthorization,
	pidfd int,
) error {
	send := func(signal unix.Signal) error {
		if expected.Lock.Owner == nil {
			return fmt.Errorf("authoritative wake owner is missing before %s", signal)
		}
		observation, err := observeAuthoritativeWakeOwner(*expected.Lock.Owner)
		if err != nil {
			return fmt.Errorf("observe authoritative wake owner before %s: %w", signal, err)
		}
		sendErr := withExistingWakeMutationScopeModeInDir(
			agentDir,
			unix.LOCK_EX|unix.LOCK_NB,
			func(scope *wakeMutationScope) error {
				if err := scope.requireCanonical(); err != nil {
					return fmt.Errorf("authorize authoritative wake before %s: %w", signal, err)
				}
				current := inspectWakeLockAt(
					scope.dirfd,
					scope.agentDir,
					expected.Root,
					expected.Agent,
				)
				if !sameWakeLockGeneration(expected, current) {
					return fmt.Errorf("authoritative wake generation changed before %s; refusing to signal", signal)
				}
				if err := validateBoundWakeMutationAt(scope.dirfd, scope.agentDir, current); err != nil {
					return fmt.Errorf("authorize authoritative wake before %s: %w", signal, err)
				}
				if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
					return fmt.Errorf("authoritative wake claim changed before %s; refusing to signal", signal)
				}
				if err := validateAuthoritativeWakeStopAuthorization(current, auth, observation); err != nil {
					return fmt.Errorf("authorize authoritative wake before %s: %w", signal, err)
				}
				return scope.sendPidfdSignal(pidfd, signal)
			},
		)
		closeErr := observation.Close()
		return errors.Join(sendErr, closeErr)
	}
	return terminateWakePidfdWithSignalAuthorization(pidfd, send)
}

func validateAuthoritativeWakeStopAuthorization(
	inspection wakeLockInspection,
	auth wakeOwnerReleaseAuthorization,
	observation wakeOwnerObservation,
) (retErr error) {
	if auth.Rollback {
		return fmt.Errorf("authoritative wake rollback stop is unsupported on Linux")
	}
	if inspection.Lock.Owner == nil {
		return fmt.Errorf("authoritative wake owner is missing")
	}
	owner := *inspection.Lock.Owner
	switch observation.State {
	case wakeOwnerDead:
		return nil
	case wakeOwnerSame:
		if auth.Token == nil {
			return fmt.Errorf("wake owner token is missing")
		}
		if err := validateAuthoritativeWakeOwner(*auth.Token); err != nil {
			return fmt.Errorf("wake owner token is invalid: %w", err)
		}
		if !sameWakeOwner(auth.Token, &owner) {
			return fmt.Errorf("wake owner token does not match the claim")
		}
		callerSession, err := getWakeProcessSID(os.Getpid())
		if err != nil {
			return fmt.Errorf("wake stop caller OS session unavailable: %w", err)
		}
		if callerSession != owner.SessionID {
			return fmt.Errorf(
				"wake stop caller OS session %d does not match owner session %d",
				callerSession,
				owner.SessionID,
			)
		}
		return nil
	default:
		return fmt.Errorf("wake owner is unknown: %s", observation.Reason)
	}
}

func terminateWakePidfdWithSignalAuthorization(
	pidfd int,
	send func(unix.Signal) error,
) error {
	if err := send(unix.SIGTERM); err != nil {
		return err
	}
	exited, err := linuxPidfdPoll(pidfd, wakeTerminateGrace)
	if err != nil {
		return fmt.Errorf("poll pidfd after SIGTERM: %w", err)
	}
	if exited {
		return nil
	}
	if err := send(unix.SIGKILL); err != nil {
		return err
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
