//go:build darwin

package cli

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

var signalWakeProcess = func(pid int, sig os.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLockWithRawConsent(inspection, false)
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
		if err := terminateWakeProcess(recheck); err != nil {
			return resolveMissingWakeLockAfterTermination(recheck, err)
		}
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
	if err := signalWakeProcess(inspection.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal wake process SIGTERM: %w", err)
	}
	time.Sleep(wakeTerminateGrace)
	switch state := inspectWakeIdentity(inspection); state {
	case wakeIdentityGoneOrDifferent:
		return nil
	case wakeIdentityUnknown:
		return fmt.Errorf("wake process identity is unknown after SIGTERM; preserving wake lock")
	}
	if !sameConfirmedWakeLock(inspection) {
		return fmt.Errorf("wake process identity changed before SIGKILL")
	}
	if err := signalWakeProcess(inspection.PID, syscall.SIGKILL); err != nil {
		return fmt.Errorf("signal wake process SIGKILL: %w", err)
	}
	deadline := time.Now().Add(wakeTerminateGrace)
	for {
		switch state := inspectWakeIdentity(inspection); state {
		case wakeIdentityGoneOrDifferent:
			return nil
		case wakeIdentityUnknown:
			return fmt.Errorf("wake process identity is unknown after SIGKILL; preserving wake lock")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wake process still alive after SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sameConfirmedWakeLock(inspection wakeLockInspection) bool {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	return sameWakeLockInspection(inspection, recheck) && recheck.IdentityConfirmed
}
