//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const wakePreparedFileName = ".wake.prepared"

const wakePreparedPollInterval = 25 * time.Millisecond

var waitForWakePreparedRetry = sleepUntilWakePreparedRetry
var afterGenericWakeLockRemoval = func(int, *wakeAgentDir) error { return nil }

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

func writeWakePreparedFileInDir(
	agentDir *wakeAgentDir,
	root, me string,
	expected wakeLockInspection,
) error {
	if agentDir == nil {
		return fmt.Errorf("wake agent directory capability is missing")
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !sameWakeLockGeneration(expected, current) {
			return fmt.Errorf("wake lock generation changed before preparation publication")
		}
		marker := wakeReady{
			Schema:       wakeReadySchema,
			Generation:   current.Lock.Generation,
			TargetDigest: current.Lock.TargetDigest,
		}
		if err := validateWakeReadyLockAndTargetAt(dirfd, agentDir, root, me, current, marker); err != nil {
			return err
		}
		if err := writeWakeGenerationFileAt(dirfd, wakePreparedFileName, "wake prepared marker", marker); err != nil {
			return err
		}
		if err := reconcileWakeStateAfterLegacyMutationAt(dirfd, agentDir, root, me); err != nil {
			if continueAfterWakeStateProjectionError(err) {
				return nil
			}
			return fmt.Errorf("wake prepared marker committed; refresh wake state: %w", err)
		}
		return nil
	})
}

func freezeGenericWakePreparedCleanupAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	current wakeLockInspection,
) (*wakeGenerationFileSnapshot, error) {
	snapshot, exists, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		wakePreparedFileName,
		"wake prepared marker",
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot wake prepared marker before cleanup: %w", err)
	}
	if !exists || snapshot.Marker.Generation != current.Lock.Generation {
		return nil, nil
	}
	if err := validateWakeReadyLockAndTargetAt(
		dirfd,
		agentDir,
		root,
		me,
		current,
		snapshot.Marker,
	); err != nil {
		return nil, fmt.Errorf("validate wake prepared marker before cleanup: %w", err)
	}
	return &snapshot, nil
}

func validateWakePreparedFileAgainstInspection(root, me string, current wakeLockInspection) (bool, error) {
	selection, err := readWakeStateSelection(root, me)
	if err != nil {
		return false, err
	}
	return validateWakePreparedSelection(current, selection)
}

func validateWakePreparedFileAgainstInspectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	current wakeLockInspection,
) (bool, error) {
	selection, err := readWakeStateSelectionAt(dirfd, agentDir, root, me)
	if err != nil {
		return false, err
	}
	return validateWakePreparedSelection(current, selection)
}

func validateWakePreparedSelection(
	current wakeLockInspection,
	selection wakeStateReadSelection,
) (bool, error) {
	if selection.PreparedErr != nil {
		return false, selection.PreparedErr
	}
	var prepared *wakeStatePrepared
	if selection.PreparedPresent {
		prepared = &wakeStatePrepared{
			Schema:       selection.Prepared.Schema,
			Generation:   selection.Prepared.Generation,
			TargetDigest: selection.Prepared.TargetDigest,
		}
	}
	observation := classifyWakeStatePrepared(
		prepared,
		current.Lock.Generation,
		current.Lock.TargetDigest,
	)
	if observation == wakeStatePreparedAbsent || observation == wakeStatePreparedStale {
		// A missing marker or one from a previous generation is not preparation.
		return false, nil
	}
	if err := validateWakeReadyLockAndSelectedTarget(
		current,
		selection.Prepared,
		selection.Target,
		selection.TargetPresent,
	); err != nil {
		return false, fmt.Errorf("existing amq wake prepared marker is not valid: %w", err)
	}
	return observation == wakeStatePreparedCurrent, nil
}

func writeWakeReadyFileForPreparedWake(root, me, path string, expected wakeLockInspection, deadline time.Time) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	publication, err := writeWakeReadyFileForPreparedWakeInDir(
		agentDir,
		root,
		me,
		path,
		expected,
		deadline,
	)
	if publication != nil {
		_ = publication.Close()
	}
	return err
}

func writeWakeReadyFileForPreparedWakeInDir(
	agentDir *wakeAgentDir,
	root, me, path string,
	expected wakeLockInspection,
	deadline time.Time,
) (*wakeReadyPublication, error) {
	if agentDir == nil {
		return nil, fmt.Errorf("wake agent directory capability is missing")
	}
	for {
		if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
			return nil, err
		}
		prepared := false
		var publication *wakeReadyPublication
		err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			current := inspectWakeLockAt(dirfd, agentDir, root, me)
			if !sameWakeLockGeneration(expected, current) {
				return fmt.Errorf("wake lock generation changed before existing-wake readiness publication")
			}
			if !confirmedLiveWake(current) {
				return fmt.Errorf("existing amq wake stopped before preparation completed")
			}
			if err := validateWakeReadyLockAndTargetAt(dirfd, agentDir, root, me, current, wakeReady{
				Schema:       wakeReadySchema,
				Generation:   current.Lock.Generation,
				TargetDigest: current.Lock.TargetDigest,
			}); err != nil {
				return fmt.Errorf("existing amq wake became incompatible before preparation completed: %w", err)
			}
			var err error
			prepared, err = validateWakePreparedFileAgainstInspectionAt(
				dirfd,
				agentDir,
				root,
				me,
				current,
			)
			if err != nil || !prepared {
				return err
			}
			marker := wakeReady{
				Schema:       wakeReadySchema,
				Generation:   current.Lock.Generation,
				TargetDigest: current.Lock.TargetDigest,
			}
			publication, err = publishWakeReadyFile(path, marker)
			return err
		})
		if err != nil {
			return nil, err
		}
		if prepared {
			if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
				cleanupErr := publication.removeIfUnchanged()
				closeErr := publication.Close()
				return nil, errors.Join(err, cleanupErr, closeErr)
			}
			return publication, nil
		}
		if !waitForWakePreparedRetry(deadline) {
			return nil, fmt.Errorf("existing amq wake did not publish its prepared marker before the readiness deadline")
		}
	}
}

func sleepUntilWakePreparedRetry(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	delay := wakePreparedPollInterval
	if remaining < delay {
		delay = remaining
	}
	time.Sleep(delay)
	// Let the caller perform one final guarded inspection at the deadline.
	return true
}
