//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Test entry points that wrap production functions with the defaults and
// directory handling tests use. Production calls the wrapped functions
// directly.

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string, target *wakeTarget) (cleanup func(), err error) {
	return acquireWakeLockWithOptions(root, me, wakeLockAcquireOptions{target: target})
}

func acquireWakeLockWithOptions(root, me string, options wakeLockAcquireOptions) (cleanup func(), err error) {
	agentDir, innerCleanup, err := acquireWakeLockWithOptionsRetained(root, me, options)
	if err != nil {
		return nil, err
	}
	return func() {
		innerCleanup()
		_ = agentDir.Close()
	}, nil
}

func acquireWakeLockWithOptionsRetained(
	root, me string,
	options wakeLockAcquireOptions,
) (agentDir *wakeAgentDir, cleanup func(), err error) {
	agentDir, err = openWakeAgentDir(root, me)
	if err != nil {
		return nil, nil, err
	}
	innerCleanup, err := acquireWakeLockWithOptionsInDir(agentDir, root, me, options)
	if err != nil {
		_ = agentDir.Close()
		return nil, nil, err
	}
	return agentDir, innerCleanup, nil
}

func shouldReplaceOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	replace, err := wakeLockReplacementNeeded(inspection)
	if err != nil || !replace {
		return replace, err
	}
	return terminateAndRemoveOrphanedWakeLock(inspection)
}

func acquireAuthoritativeWakeLockWithOptions(
	root string,
	me string,
	options wakeLockAcquireOptions,
) (cleanup func(), retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, root, me) }()
	if err := os.MkdirAll(fsq.AgentBase(root, me), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create agent directory: %w", err)
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentDir.Close() }()
	return acquireAuthoritativeWakeLockWithOptionsInDirAndStopPreparer(
		agentDir,
		root,
		me,
		options,
		prepareAuthoritativeWakeStop,
	)
}

func publishAuthoritativeWakeClaimAt(
	scope *wakeMutationScope,
	root string,
	me string,
	target wakeTarget,
	lock wakeLock,
) error {
	return publishAuthoritativeWakeClaimWithDebugAt(scope, root, me, target, lock, false)
}

func writeWakeTarget(root, me string, target wakeTarget) error {
	agentBase := fsq.AgentBase(root, me)
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return fmt.Errorf("create wake target directory: %w", err)
	}
	return withWakeLifecycleGuard(root, me, func() error {
		if err := validateWakeTarget(target, root, me); err != nil {
			return err
		}
		data, err := json.MarshalIndent(target, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal wake target: %w", err)
		}
		data = append(data, '\n')
		return writeWakeMetadataFile(wakeTargetPath(root, me), data, "wake target")
	})
}

func withWakeLifecycleGuard(root, me string, fn func() error) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeLifecycleGuardInDir(agentDir, wakeGuardWait, func(int) error { return fn() })
}

func wakeLifecycleGuardPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeLifecycleGuardFileName)
}

func wakePreparedPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakePreparedFileName)
}

func writeWakePreparedFile(root, me string, expected wakeLockInspection) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return writeWakePreparedFileInDir(agentDir, root, me, expected)
}

func validateWakePreparedFileAgainstInspection(root, me string, current wakeLockInspection) (bool, error) {
	selection, err := readWakeStateSelectionForInspection(root, me, current)
	if err != nil {
		return false, err
	}
	return validateWakePreparedSelection(current, selection)
}

func writeWakeReadyFile(root, me, path string, expected wakeLockInspection) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return writeWakeReadyFileAgainstOwnerInDir(
		agentDir,
		root,
		me,
		path,
		expected,
		nil,
	)
}

func writeWakeRepairFloor(root, me string, floor wakeRepairFloor) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return writeWakeRepairFloorInDir(agentDir, root, floor)
}

func authorizeTerminalWritePlatform(cfg *wakeConfig) bool {
	allowed, _ := authorizeTerminalWritePlatformState(cfg)
	return allowed
}

func bindWakeRestartCandidate(candidate wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
	if err := validateWakeImageEvidence(candidate); err != nil {
		return nil, err
	}
	return bindWakeRestartCandidatePlatform(candidate)
}

func cleanupTerminatedWakeLock(expected wakeLockInspection) error {
	agentDir, err := openExistingCoopWakeAgentDir(expected.Root, expected.Agent)
	if err != nil || agentDir == nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return cleanupTerminatedWakeLockInDir(agentDir, expected)
}

func readWakeGenerationFileAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) (wakeReady, bool, error) {
	snapshot, exists, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		name,
		label,
	)
	return snapshot.Marker, exists, err
}
