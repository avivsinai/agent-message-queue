//go:build linux

package cli

import "fmt"

// Test entry points that wrap production functions with the defaults and
// directory handling tests use. Production calls the wrapped functions
// directly.

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	agentDir, err := openExistingCoopWakeAgentDir(inspection.Root, inspection.Agent)
	if err != nil {
		return false, withWakeDiagnostic(err, inspection.Root, inspection.Agent)
	}
	if agentDir == nil {
		return false, withWakeDiagnostic(
			fmt.Errorf("wake agent directory disappeared before termination"),
			inspection.Root,
			inspection.Agent,
		)
	}
	defer func() { _ = agentDir.Close() }()
	removed, err := terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
		agentDir,
		inspection,
		false,
		nil,
	)
	return removed, withWakeDiagnostic(err, inspection.Root, inspection.Agent)
}

func planWakeRestartStagePlatform(wakeImageEvidenceV1, string) (string, error) {
	return "", nil
}
