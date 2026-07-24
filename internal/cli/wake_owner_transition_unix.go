//go:build darwin || linux

package cli

func classifyWakeClaimForGenericTransition(inspection wakeLockInspection) wakeClaimClass {
	return classifyPersistedWakeClaim(inspection)
}
