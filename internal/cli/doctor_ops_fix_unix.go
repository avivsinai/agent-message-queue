//go:build darwin || linux

package cli

func fixStaleWakeLockForDoctor(
	root string,
	agent string,
	inspection *wakeLockInspection,
	lock *opsWakeLock,
) error {
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()

	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		recheck := inspectWakeLockAt(dirfd, agentDir, root, agent)
		sameGeneration := sameWakeLockGeneration(*inspection, recheck)
		*inspection = recheck
		if !sameGeneration || recheck.Status != wakeLockStale {
			lock.Status = string(recheck.Status)
			lock.Reason = "wake lock changed before fix"
			return nil
		}
		if err := reconcileBoundWakePreparedProjectionAt(dirfd, agentDir, recheck); err != nil {
			return err
		}
		if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, recheck); err != nil {
			return err
		}
		if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, recheck); err != nil {
			return err
		}
		lock.Status = "fixed"
		lock.Removed = true
		return nil
	})
}
