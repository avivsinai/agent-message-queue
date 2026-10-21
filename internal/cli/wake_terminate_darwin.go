//go:build darwin

package cli

import (
	"fmt"
)

var (
	afterDarwinWakeSignalValidation = func() {}
)

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
) (removed bool, retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, inspection.Root, inspection.Agent) }()
	var recheck wakeLockInspection
	if err := withWakeLifecycleGuardInDir(agentDir, wakeGuardExistingNoWait, func(dirfd int) error {
		recheck = inspectWakeLockAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if !sameWakeLockInspection(inspection, recheck) {
			return nil
		}
		if err := validateWakeLockOwnerlessMutationAtForTermination(dirfd, agentDir, recheck); err != nil {
			return err
		}
		if requestedTarget != nil {
			return requireExistingWakeTargetMatchesAt(dirfd, agentDir, recheck, *requestedTarget)
		}
		return nil
	}); err != nil {
		return false, err
	}
	if !sameWakeLockInspection(inspection, recheck) || !recheck.IdentityConfirmed {
		return false, nil
	}
	if recheck.Process.Running &&
		recheck.Lock.WakeMode != wakeTargetInjectVia &&
		!wakeLockNeedsReplacement(recheck) && !allowRawOrphan {
		return false, withWakeDiagnostic(fmt.Errorf(
			"live raw wake for %s (pid %d, start %s) is not eligible for automatic replacement; retry through amq coop exec to review and consent to takeover, or stop it from its owning session; refusing to signal without consent",
			recheck.Agent,
			recheck.PID,
			recheck.Lock.ProcessStart,
		), inspection.Root, inspection.Agent)
	}
	if allowRawOrphan && recheck.Process.Running &&
		recheck.Lock.WakeMode != wakeTargetInjectVia &&
		!isLiveRawOrphan(recheck) {
		return false, fmt.Errorf("live raw wake identity changed before consented termination")
	}
	if recheck.Process.Running && recheck.Lock.WakeMode == wakeTargetInjectVia {
		if recheck.Lock.ControlSocket == "" || recheck.Lock.Generation == "" {
			return false, fmt.Errorf("live inject-via wake orphan has no cooperative control endpoint; stop the owning supervisor")
		}
		return cooperativeStopInjectViaInDir(agentDir, recheck, requestedTarget)
	}
	if recheck.Process.Running {
		// Darwin raw signaling is operator_only, so terminate* always errors here.
		// The lock-removal path below is reachable only through the cooperative stop.
		err := terminateWakeProcessInDir(agentDir, recheck)
		_, confirmErr := sameConfirmedWakeLockInDir(agentDir, recheck)
		if confirmErr != nil {
			return false, confirmErr
		}
		return false, err
	}
	removed = false
	err := withWakeMutationScopeInDir(
		agentDir,
		wakeGuardExistingNoWait,
		func(scope *wakeMutationScope) error {
			dirfd, _, err := scope.location()
			if err != nil {
				return err
			}
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
			if !sameWakeLockGenerationForRetainedTermination(recheck, current) {
				return nil
			}
			if requestedTarget != nil {
				if err := requireExistingWakeTargetMatchesAt(dirfd, agentDir, current, *requestedTarget); err != nil {
					return err
				}
			}
			if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, current); err != nil {
				return err
			}
			var removeErr error
			outcome := removeWakeLockIfUnchangedGuardedAtOutcome(
				scope,
				current,
				wakeLockRemoval{unlink: scope.unlinkWakeLockForCleanup, durable: true},
			)
			removed, removeErr = outcome.Committed, outcome.Err
			return removeErr
		},
	)
	return removed, err
}

func terminateWakeProcessInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	confirmed, err := sameConfirmedWakeLockInDir(agentDir, inspection)
	if err != nil {
		return withWakeDiagnostic(err, inspection.Root, inspection.Agent)
	}
	if !confirmed {
		return withWakeDiagnostic(
			fmt.Errorf("wake process identity changed before SIGTERM"),
			inspection.Root,
			inspection.Agent,
		)
	}
	afterDarwinWakeSignalValidation()
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
		wakeCheckRemedy(inspection.Root, inspection.Agent),
	)
}

func sameConfirmedWakeLockInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (bool, error) {
	confirmed := false
	err := withWakeLifecycleGuardInDir(agentDir, wakeGuardExistingNoWait, func(dirfd int) error {
		recheck := inspectWakeLockAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		confirmed = sameWakeLockInspection(inspection, recheck) &&
			recheck.IdentityConfirmed
		return nil
	})
	return confirmed, err
}
