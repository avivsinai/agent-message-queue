//go:build darwin

package cli

import "fmt"

// Test entry points that wrap production functions with the defaults and
// directory handling tests use. Production calls the wrapped functions
// directly.

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (removed bool, retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, inspection.Root, inspection.Agent) }()
	agentDir, err := openExistingCoopWakeAgentDir(inspection.Root, inspection.Agent)
	if err != nil {
		return false, err
	}
	if agentDir == nil {
		return false, fmt.Errorf("wake agent directory disappeared before termination")
	}
	defer func() { _ = agentDir.Close() }()
	return terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
		agentDir,
		inspection,
		false,
		nil,
	)
}

func terminateWakeProcess(inspection wakeLockInspection) error {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	if !sameWakeLockInspection(inspection, recheck) || !recheck.IdentityConfirmed {
		return withWakeDiagnostic(
			fmt.Errorf("wake process identity changed before SIGTERM"),
			inspection.Root,
			inspection.Agent,
		)
	}
	afterDarwinWakeSignalValidation()
	// Darwin has no pidfd. kill(pid) after the last identity recheck can hit a
	// recycled PID. Cooperative control is the only agent-safe stop; raw numeric
	// signaling is operator_only.
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
		wakeCheckRemedy(inspection.Root, inspection.Agent),
	)
}

func startWakeControlListener(
	root, me string,
	lock wakeLock,
) (func(), <-chan struct{}, func(), error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup, stop, markStopped, err := startWakeControlListenerInDirOwnedWithRestart(
		agentDir,
		root,
		me,
		lock,
		true,
		nil,
		nil,
	)
	if err != nil {
		_ = agentDir.Close()
	}
	return cleanup, stop, markStopped, err
}

func bindWakeRestartCandidatePlatform(candidate wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
	return bindWakeRestartCandidateAtPlatform(candidate, "")
}
