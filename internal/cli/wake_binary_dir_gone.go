package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	wakeRestartStageBinaryDirGone = "binary-dir-gone"
	wakeBinaryDirGoneMessage      = "wake started by a binary that no longer exists"
)

func wakeInspectionBinaryDirGone(inspection wakeLockInspection) bool {
	paths := []string{inspection.Lock.ImagePath}
	if inspection.Lock.RunningImageEvidence != nil {
		paths = append(paths, inspection.Lock.RunningImageEvidence.ExecutionPath)
	}
	return wakeAnyRecordedPathParentGone(paths)
}

func wakeAnyRecordedPathParentGone(paths []string) bool {
	for _, path := range paths {
		if wakeRecordedPathParentGone(path) {
			return true
		}
	}
	return false
}

// wakeRecordedPathParentGone reports that path lives under a directory that no
// longer exists (or is no longer a directory). A missing file whose parent is
// still present is not this state.
func wakeRecordedPathParentGone(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if parent == "." || parent == string(filepath.Separator) {
		return false
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return !info.IsDir()
}

func applyWakeBinaryDirGoneOpsReason(lock *opsWakeLock, inspection wakeLockInspection, stage wakeRestartStageDiagnostic) {
	if lock == nil {
		return
	}
	if wakeInspectionBinaryDirGone(inspection) ||
		stage.Status == wakeRestartStageBinaryDirGone ||
		(lock.WakeCheckDecision != nil &&
			lock.WakeCheckDecision.Action.ReasonCode == wakeReasonBinaryDirGone) {
		lock.Reason = wakeReasonBinaryDirGone
	}
}
