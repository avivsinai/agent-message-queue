//go:build !darwin && !linux

package cli

func fixStaleWakeLockForDoctor(
	root string,
	agent string,
	inspection wakeLockInspection,
	lock *opsWakeLock,
) error {
	return withWakeLifecycleGuard(root, agent, func() error {
		recheck := inspectWakeLock(root, agent)
		if !sameWakeLockGeneration(inspection, recheck) || recheck.Status != wakeLockStale {
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
