//go:build darwin || linux

package cli

import "fmt"

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
		if err := reclaimWakeRestartStageForStaleLock(record); err != nil {
			return fmt.Errorf("reclaim persisted wake restart stage: %w", err)
		}
		quarantine = &snapshot
	}
	if err := reclaimWakeRestartRunningImagePlatform(lock.Lock); err != nil {
		return fmt.Errorf("reclaim wake running stage before lock removal: %w", err)
	}
	if quarantine != nil {
		if _, err := quarantineWakeRestartRecordAt(dirfd, agentDir, *quarantine); err != nil {
			return fmt.Errorf("quarantine wake restart state before lock removal: %w", err)
		}
	}
	return nil
}
