//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func planWakeRestartStagePlatform(candidate wakeImageEvidenceV1, requestID string) (string, error) {
	if !validWakeReloadTransportGeneration(requestID) {
		return "", fmt.Errorf("wake restart stage request id is invalid")
	}
	if err := validateWakeImageEvidenceForPlatform(candidate, "darwin"); err != nil {
		return "", err
	}
	base := filepath.Base(candidate.ExecutionPath)
	dir := filepath.Join(filepath.Dir(candidate.ExecutionPath), "."+base+".amq-restart-"+requestID)
	return filepath.Join(dir, base), nil
}

func validateWakeRestartStageStatePlatform(record wakeRestartRecord) error {
	if record.PreviousBoundImage != nil {
		if err := validateWakeImageEvidenceForPlatform(*record.PreviousBoundImage, "darwin"); err != nil {
			return fmt.Errorf("previous wake restart bound stage is invalid: %w", err)
		}
		if !validPreviousWakeRestartBoundImagePlatform(*record.PreviousBoundImage) {
			return fmt.Errorf("previous wake restart bound stage is not an owned Darwin stage")
		}
	}
	// Records written before persisted stage ownership was introduced, and the
	// schema-2 successor claims derived from them, have no stage fields. They
	// remain readable so an in-flight v0.57.2 restart can finish after upgrade.
	if record.StagePath == "" && record.BoundImage == nil {
		return nil
	}
	planned, err := planWakeRestartStagePlatform(record.Candidate, record.RequestID)
	if err != nil {
		return err
	}
	if record.StagePath != planned {
		return fmt.Errorf("wake restart stage path does not match the exact request")
	}
	if record.BoundImage == nil {
		return nil
	}
	if err := validateWakeImageEvidenceForPlatform(*record.BoundImage, "darwin"); err != nil {
		return fmt.Errorf("wake restart bound stage is invalid: %w", err)
	}
	if record.BoundImage.ExecutionPath != record.StagePath ||
		!sameRequestedAndBoundWakeImageEvidence(record.Candidate, *record.BoundImage) {
		return fmt.Errorf("wake restart bound stage does not match the planned request")
	}
	if record.PreviousBoundImage != nil &&
		record.PreviousBoundImage.ExecutionPath == record.BoundImage.ExecutionPath {
		return fmt.Errorf("wake restart previous and current stages use the same path")
	}
	return nil
}

func reclaimWakeRestartStagePlatform(record wakeRestartRecord) error {
	if err := validateWakeRestartStageStatePlatform(record); err != nil {
		return err
	}
	if record.PreviousBoundImage != nil {
		if err := cleanupPersistedDarwinWakeRestartStage(*record.PreviousBoundImage); err != nil {
			return fmt.Errorf("reclaim previous Darwin wake restart stage: %w", err)
		}
	}
	if record.StagePath == "" {
		return nil
	}
	if record.BoundImage != nil {
		return cleanupPersistedDarwinWakeRestartStage(*record.BoundImage)
	}

	info, err := os.Lstat(record.StagePath)
	if errors.Is(err, os.ErrNotExist) {
		return removeEmptyDarwinWakeRestartStageDir(record.StagePath)
	}
	if err != nil {
		return fmt.Errorf("stat planned Darwin wake restart stage: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse reclaim of replaced Darwin wake restart stage")
	}
	fd, err := unix.Open(record.StagePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open planned Darwin wake restart stage: %w", err)
	}
	file := os.NewFile(uintptr(fd), record.StagePath)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("bind planned Darwin wake restart stage")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("planned Darwin wake restart stage changed before reclaim")
	}
	bound, err := captureWakeImageEvidenceFromOpenFile(
		file,
		record.StagePath,
		record.Candidate.EmbeddedVersion,
		wakeImageMethodPathnameExecObserved,
	)
	if err != nil {
		return err
	}
	if !sameRequestedAndBoundWakeImageEvidence(record.Candidate, bound) {
		return fmt.Errorf("refuse reclaim of Darwin stage that does not match the persisted request")
	}
	return cleanupDarwinWakeRestartStage(bound, true)
}

func cleanupPersistedDarwinWakeRestartStage(bound wakeImageEvidenceV1) error {
	if err := cleanupDarwinWakeRestartStage(bound, true); err != nil {
		return err
	}
	return removeEmptyDarwinWakeRestartStageDir(bound.ExecutionPath)
}

func validateWakeRestartPersistedBoundPlatform(
	record wakeRestartRecord,
	bootstrap wakeImageEvidenceV1,
) error {
	if record.BoundImage == nil {
		if record.Schema == wakeRestartSchemaV1 && record.StagePath == "" &&
			bootstrap.Method == wakeImageMethodPathnameExecVerified {
			return nil
		}
		return fmt.Errorf("darwin wake restart record has no persisted bound stage")
	}
	if !sameDarwinStagedWakeImageEvidence(*record.BoundImage, bootstrap) {
		return fmt.Errorf("darwin wake restart persisted stage does not match the exec bootstrap")
	}
	return nil
}

func removeEmptyDarwinWakeRestartStageDir(stagePath string) error {
	dirPath := filepath.Dir(stagePath)
	parentPath := filepath.Dir(dirPath)
	name := filepath.Base(dirPath)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open Darwin wake restart stage parent: %w", err)
	}
	defer func() { _ = unix.Close(parentFD) }()
	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, syscall.ENOENT) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat Darwin wake restart stage directory: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR || before.Mode&0o777 != 0o700 || before.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("refuse reclaim of replaced Darwin wake restart stage directory")
	}
	dirFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open Darwin wake restart stage directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirFD), dirPath)
	if dir == nil {
		_ = unix.Close(dirFD)
		return fmt.Errorf("bind Darwin wake restart stage directory")
	}
	entries, readErr := dir.ReadDir(1)
	_ = dir.Close()
	if !errors.Is(readErr, io.EOF) || len(entries) != 0 {
		return fmt.Errorf("refuse reclaim of non-empty Darwin wake restart stage directory")
	}
	var after unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		before.Dev != after.Dev || before.Ino != after.Ino || before.Ctim != after.Ctim {
		return fmt.Errorf("darwin wake restart stage directory changed before reclaim")
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("remove empty Darwin wake restart stage directory: %w", err)
	}
	return nil
}

func wakeRestartStageExistsPlatform(record wakeRestartRecord) (bool, error) {
	if err := validateWakeRestartStageStatePlatform(record); err != nil {
		return false, err
	}
	if record.StagePath == "" {
		return false, nil
	}
	if _, err := os.Lstat(record.StagePath); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(filepath.Dir(record.StagePath)); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func reclaimWakeRestartRunningImagePlatform(lock wakeLock) error {
	if lock.RunningImageEvidence == nil ||
		!validPreviousWakeRestartBoundImagePlatform(*lock.RunningImageEvidence) {
		return nil
	}
	return cleanupPersistedDarwinWakeRestartStage(*lock.RunningImageEvidence)
}
