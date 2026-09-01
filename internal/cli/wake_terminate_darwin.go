//go:build darwin

package cli

import (
	"fmt"
)

var (
	afterDarwinWakeSignalValidation = func() {}
)

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockWithRawConsent(inspection, false)
}

func terminateAndRemoveOrphanedWakeLockWithRawConsent(
	inspection wakeLockInspection,
	allowRawOrphan bool,
) (removed bool, retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, inspection.Root, inspection.Agent) }()
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
) (removed bool, retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, inspection.Root, inspection.Agent) }()
	var recheck wakeLockInspection
	if err := withExistingWakeLifecycleGuardNoWaitInDir(agentDir, func(dirfd int) error {
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
	err := withExistingWakeMutationScopeNoWaitInDir(
		agentDir,
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
			outcome := removeWakeLockIfUnchangedGuardedAtDurableOutcome(
				scope,
				current,
				scope.unlinkWakeLockForCleanup,
			)
			removed, removeErr = outcome.Committed, outcome.Err
			return removeErr
		},
	)
	return removed, err
}

func terminateWakeProcess(inspection wakeLockInspection) error {
	if !sameConfirmedWakeLock(inspection) {
		return withWakeDiagnostic(
			fmt.Errorf("wake process identity changed before SIGTERM"),
			inspection.Root,
			inspection.Agent,
		)
	}
	afterDarwinWakeSignalValidation()
	// Darwin has no pidfd. kill(pid) after the last identity recheck can hit a
	// recycled PID. Cooperative control is the only agent-safe stop; raw numeric
	// signaling is operator_only.
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
		wakeCheckRemedy(inspection.Root, inspection.Agent),
	)
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
	err := withExistingWakeLifecycleGuardNoWaitInDir(agentDir, func(dirfd int) error {
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

func sameConfirmedWakeLock(inspection wakeLockInspection) bool {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	return sameWakeLockInspection(inspection, recheck) && recheck.IdentityConfirmed
}
