//go:build linux

package cli

import "fmt"

func planWakeRestartStagePlatform(wakeImageEvidenceV1, string) (string, error) {
	return "", nil
}

func planWakeRestartStageForRecordPlatform(wakeRestartRecord) (string, error) {
	return "", nil
}

func validateWakeRestartStageStatePlatform(record wakeRestartRecord) error {
	if record.StagePath != "" || record.BoundImage != nil {
		return fmt.Errorf("linux wake restart record contains Darwin stage state")
	}
	return nil
}

func reclaimWakeRestartStagePlatform(record wakeRestartRecord) error {
	return validateWakeRestartStageStatePlatform(record)
}

func validateWakeRestartPersistedBoundPlatform(record wakeRestartRecord, _ wakeImageEvidenceV1) error {
	if record.BoundImage != nil {
		return fmt.Errorf("linux wake restart record contains a persisted Darwin bound stage")
	}
	return nil
}

func wakeRestartStageExistsPlatform(wakeRestartRecord) (bool, error) {
	return false, nil
}

func wakeRestartStageUsesStableStatePlatform(string) (bool, error) { return false, nil }

func reclaimWakeRestartRunningImagePlatform(wakeLock) error { return nil }
