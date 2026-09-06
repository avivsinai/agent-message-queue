//go:build !darwin && !linux

package cli

import "fmt"

func fixStaleWakeLockForDoctor(
	root string,
	agent string,
	inspection *wakeLockInspection,
	lock *opsWakeLock,
) error {
	return withWakeLifecycleGuard(root, agent, func() error {
		recheck := inspectWakeLock(root, agent)
		sameGeneration := sameWakeLockGeneration(*inspection, recheck)
		*inspection = recheck
		if !sameGeneration || recheck.Status != wakeLockStale {
			lock.Status = string(recheck.Status)
			lock.Reason = "wake lock changed before fix"
			return nil
		}
		if err := validateWakeLockStaleRemoval(recheck); err != nil {
			return err
		}
		if err := removeWakeLockIfUnchangedGuarded(recheck); err != nil {
			return err
		}
		lock.Status = "fixed"
		lock.Removed = true
		return nil
	})
}

func validateWakeLockStaleRemoval(inspection wakeLockInspection) error {
	if _, err := readWakeStateSelectionForInspection(
		inspection.Root,
		inspection.Agent,
		inspection,
	); err != nil {
		return err
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return fmt.Errorf("owner-bound wake claims require %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
	}
	if err := validateWakeLockRepairable(inspection); err == nil {
		return nil
	} else if inspection.Status != wakeLockStale {
		return err
	}
	// Identity mismatches reach stale only when the tri-state classifier has
	// affirmative proof that the recorded generation is gone or different.
	return nil
}
