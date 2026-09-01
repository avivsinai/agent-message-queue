//go:build darwin || linux

package cli

func removeWakeLockIfUnchanged(inspection wakeLockInspection) error {
	agentDir, err := openWakeAgentDir(inspection.Root, inspection.Agent)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return removeWakeLockIfUnchangedGuardedAt(scope, inspection)
	})
}
