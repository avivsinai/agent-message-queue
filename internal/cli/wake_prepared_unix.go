//go:build darwin || linux

package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const wakePreparedFileName = ".wake.prepared"

const wakePreparedPollInterval = 25 * time.Millisecond

var waitForWakePreparedRetry = sleepUntilWakePreparedRetry

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
		// The marker intentionally persists after exit; its generation binding
		// makes stale files unusable by later wake instances.
		return writeWakeGenerationFileAt(dirfd, wakePreparedFileName, "wake prepared marker", marker)
	})
}

func validateWakePreparedFileAgainstInspection(root, me string, current wakeLockInspection) (bool, error) {
	marker, exists, err := readWakeGenerationFile(wakePreparedPath(root, me), "wake prepared marker")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if marker.Generation != current.Lock.Generation {
		// Persistent marker from the previous wake generation: this exact wake
		// has not published preparation yet, so its caller should keep polling.
		return false, nil
	}
	if err := validateWakeReadyLockAndTarget(root, me, current, marker); err != nil {
		return false, fmt.Errorf("existing amq wake prepared marker is not valid: %w", err)
	}
	return true, nil
}

func validateWakePreparedFileAgainstInspectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	current wakeLockInspection,
) (bool, error) {
	marker, exists, err := readWakeGenerationFileAt(
		dirfd,
		agentDir,
		wakePreparedFileName,
		"wake prepared marker",
	)
	if err != nil {
		return false, err
	}
	if !exists || marker.Generation != current.Lock.Generation {
		return false, nil
	}
	if err := validateWakeReadyLockAndTargetAt(dirfd, agentDir, root, me, current, marker); err != nil {
		return false, fmt.Errorf("existing amq wake prepared marker is not valid: %w", err)
	}
	return true, nil
}

func writeWakeReadyFileForPreparedWake(root, me, path string, expected wakeLockInspection, deadline time.Time) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	for {
		prepared := false
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
			if current.Lock.WakeMode == wakeOwnerWakeMode {
				prepared, err = validateWakePreparedFileAgainstInspectionAt(
					dirfd,
					agentDir,
					root,
					me,
					current,
				)
			} else {
				prepared, err = validateWakePreparedFileAgainstInspection(root, me, current)
			}
			if err != nil || !prepared {
				return err
			}
			marker := wakeReady{
				Schema:       wakeReadySchema,
				Generation:   current.Lock.Generation,
				TargetDigest: current.Lock.TargetDigest,
			}
			return writeWakeGenerationFile(path, "wake ready file", marker)
		})
		if err != nil {
			return err
		}
		if prepared {
			return nil
		}
		if !waitForWakePreparedRetry(deadline) {
			return fmt.Errorf("existing amq wake did not publish its prepared marker before the readiness deadline")
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
