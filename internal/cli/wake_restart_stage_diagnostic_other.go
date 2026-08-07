//go:build !darwin && !linux

package cli

type wakeRestartStageDiagnostic struct {
	Path   string
	Status string
	Reason string
}

func diagnoseWakeRestartStage(
	string,
	string,
	wakeLockInspection,
) wakeRestartStageDiagnostic {
	return wakeRestartStageDiagnostic{}
}

func fixWakeRestartResidueWithoutLock(string, string) error {
	return nil
}

func reclaimWakeRestartStateForGuardedLockRemoval(wakeLockInspection) error {
	return nil
}
