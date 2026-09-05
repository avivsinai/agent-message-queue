//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
)

type wakeRestartStageIdentityError struct {
	path string
}

func (err *wakeRestartStageIdentityError) Error() string {
	return fmt.Sprintf("refuse cleanup of changed Darwin wake restart stage %q", err.path)
}

// A dead wake's lock and its staged executable have independent identities.
// Releasing the exact stale lock does not require authority to delete a stage
// whose persisted identity no longer agrees (for example after a remount).
func preserveChangedWakeRestartStage(err error) bool {
	var changed *wakeRestartStageIdentityError
	if !errors.As(err, &changed) {
		return false
	}
	_ = writeStderr("warning: preserving restart stage %q: saved file identity no longer matches; wake cleanup will continue\n", changed.path)
	return true
}

var reclaimWakeRestartStageForStaleLock = reclaimWakeRestartStagePlatform

func reclaimWakeRestartStateForLockRemovalAt(
	dirfd int,
	agentDir *wakeAgentDir,
	lock wakeLockInspection,
) error {
	snapshot, exists, readErr := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	var quarantine *wakeRestartRecordSnapshot
	if readErr != nil {
		if !exists || snapshot.Object.FileInfo == nil {
			return fmt.Errorf("inspect wake restart state before lock removal: %w", readErr)
		}
		quarantine = &snapshot
	} else if exists {
		record := snapshot.Record
		boundToLock := record.Generation == lock.Lock.Generation ||
			record.SuccessorGeneration == lock.Lock.Generation
		if boundToLock && record.Schema == wakeRestartSchemaV2 && lock.Status != wakeLockStale {
			return fmt.Errorf(
				"live wake restart successor handoff is preserved before lock removal",
			)
		}
		if err := reclaimWakeRestartStageForStaleLock(record); err != nil &&
			(lock.Status != wakeLockStale || !preserveChangedWakeRestartStage(err)) {
			return fmt.Errorf("reclaim persisted wake restart stage: %w", err)
		}
		quarantine = &snapshot
	}
	if err := reclaimWakeRestartRunningImagePlatform(lock.Lock); err != nil &&
		(lock.Status != wakeLockStale || !preserveChangedWakeRestartStage(err)) {
		return fmt.Errorf("reclaim wake running stage before lock removal: %w", err)
	}
	if quarantine != nil {
		if _, err := quarantineWakeRestartRecordAt(dirfd, agentDir, *quarantine); err != nil {
			return fmt.Errorf("quarantine wake restart state before lock removal: %w", err)
		}
	}
	return nil
}
