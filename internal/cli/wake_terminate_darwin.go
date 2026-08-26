//go:build darwin

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var (
	signalWakeProcess = func(pid int, sig os.Signal) error {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Signal(sig)
	}
	afterDarwinWakeSignalValidation = func() {}
)

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockWithRawConsent(inspection, false)
}

func terminateAndRemoveOrphanedWakeLockInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (bool, error) {
	var recheck wakeLockInspection
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		recheck = inspectWakeLockAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if !sameWakeLockInspection(inspection, recheck) {
			return nil
		}
		return validateWakeLockOwnerlessMutationAt(dirfd, agentDir, recheck)
	}); err != nil {
		return false, err
	}
	if !sameWakeLockInspection(inspection, recheck) || !recheck.IdentityConfirmed {
		return false, nil
	}
	if recheck.Process.Running &&
		recheck.Lock.WakeMode != wakeTargetInjectVia &&
		!wakeLockNeedsReplacement(recheck) {
		return false, fmt.Errorf(
			"live raw wake for %s (pid %d, start %s) is not eligible for automatic replacement; retry through amq coop exec to review and consent to takeover, or stop it from its owning session; refusing to signal without consent",
			recheck.Agent,
			recheck.PID,
			recheck.Lock.ProcessStart,
		)
	}
	if recheck.Process.Running && recheck.Lock.WakeMode == wakeTargetInjectVia {
		if recheck.Lock.ControlSocket == "" || recheck.Lock.Generation == "" {
			return false, fmt.Errorf("live inject-via wake orphan has no cooperative control endpoint; stop the owning supervisor")
		}
		return cooperativeStopInjectViaInDir(agentDir, recheck)
	}
	if recheck.Process.Running {
		// Darwin raw signaling is operator_only, so terminate* always errors here.
		// The lock-removal path below is reachable only through the cooperative stop.
		err := terminateWakeProcessInDir(agentDir, recheck)
		if !sameConfirmedWakeLockInDir(agentDir, recheck) {
			return true, nil
		}
		return false, err
	}
	removed := false
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
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
		if !sameWakeLockGeneration(recheck, current) {
			return nil
		}
		if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, current); err != nil {
			return err
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
	return removed, err
}

func retireLiveRawOrphan(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockWithRawConsent(inspection, true)
}

func terminateAndRemoveOrphanedWakeLockWithRawConsent(
	inspection wakeLockInspection,
	allowRawOrphan bool,
) (bool, error) {
	var recheck wakeLockInspection
	if err := withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		recheck = inspectWakeLock(inspection.Root, inspection.Agent)
		if !sameWakeLockInspection(inspection, recheck) {
			return nil
		}
		return validateWakeLockOwnerlessMutation(recheck)
	}); err != nil {
		return false, err
	}
	if !sameWakeLockInspection(inspection, recheck) || !recheck.IdentityConfirmed {
		return false, nil
	}
	if recheck.Process.Running && recheck.Lock.WakeMode != wakeTargetInjectVia {
		allowed := wakeLockNeedsReplacement(recheck)
		if allowRawOrphan {
			allowed = isLiveRawOrphan(recheck)
		}
		if !allowed {
			return false, fmt.Errorf("live raw wake for %s (pid %d, start %s) is not eligible for automatic replacement; retry through amq coop exec to review and consent to takeover, or stop it from its owning session; refusing to signal without consent", recheck.Agent, recheck.PID, recheck.Lock.ProcessStart)
		}
	}
	if recheck.Process.Running && recheck.Lock.WakeMode == wakeTargetInjectVia {
		return cooperativeStopInjectVia(recheck)
	}
	// Process termination can wait. It must happen after releasing the guard.
	if recheck.Process.Running {
		// Darwin raw signaling is operator_only, so terminate* always errors here.
		// The lock-removal path below is reachable only through the cooperative stop.
		return resolveMissingWakeLockAfterTermination(recheck, terminateWakeProcess(recheck))
	}
	removed := false
	err := withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		current := inspectWakeLock(inspection.Root, inspection.Agent)
		if !current.Exists {
			removed = true
			return nil
		}
		if !sameWakeLockGeneration(recheck, current) {
			return nil
		}
		if err := validateWakeLockStaleRemoval(current); err != nil {
			return err
		}
		if err := removeWakeLockIfUnchangedGuarded(current); err != nil {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}

func terminateWakeProcess(inspection wakeLockInspection) error {
	if !sameConfirmedWakeLock(inspection) {
		return fmt.Errorf("wake process identity changed before SIGTERM")
	}
	afterDarwinWakeSignalValidation()
	// Darwin has no pidfd. kill(pid) after the last identity recheck can hit a
	// recycled PID. Cooperative control is the only agent-safe stop; raw numeric
	// signaling is operator_only.
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
	)
}

func terminateWakeProcessInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	if !sameConfirmedWakeLockInDir(agentDir, inspection) {
		return fmt.Errorf("wake process identity changed before SIGTERM")
	}
	afterDarwinWakeSignalValidation()
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
	)
}

func sameConfirmedWakeLockInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) bool {
	confirmed := false
	_ = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
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
	return confirmed
}

func sameConfirmedWakeLock(inspection wakeLockInspection) bool {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	return sameWakeLockInspection(inspection, recheck) && recheck.IdentityConfirmed
}
