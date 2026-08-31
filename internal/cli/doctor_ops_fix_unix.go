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

	return withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		recheck := inspectWakeLockAt(dirfd, agentDir, root, agent)
		sameGeneration := sameWakeLockGeneration(*inspection, recheck)
		*inspection = recheck
		if !sameGeneration || recheck.Status != wakeLockStale {
			lock.Status = string(recheck.Status)
			lock.Reason = "wake lock changed before fix"
			return nil
		}
		if err := reconcileBoundWakePreparedProjectionAt(scope, recheck); err != nil {
			return err
		}
		if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, recheck); err != nil {
			return err
		}
		committed, err := removeWakeLockIfUnchangedGuardedAtStatus(scope, recheck)
		if committed {
			lock.Removed = true
		}
		if err != nil {
			return err
		}
		lock.Status = "fixed"
		return nil
	})
}
