//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const wakeStartedTimestampUncertainty = time.Second

const deletedWakeImageReason = "wake is running a deleted image; restart it"

// darwinWakeMappedImageGone reports whether Darwin could not name the live
// process image because the vnode is already gone. Homebrew Cellar unlinks
// produce ESRCH from proc_pidpath while the wake is still running; ENOENT is
// the same class. Other inspection failures stay unknown.
func darwinWakeMappedImageGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH)
}

func inspectWakeBinaryStalenessPlatform(
	inspection wakeLockInspection,
	current resolvedWakeBinary,
) (wakeBinaryStaleness, error) {
	started, err := time.Parse(time.RFC3339Nano, inspection.Lock.Started)
	if err != nil {
		return wakeBinaryStaleness{}, fmt.Errorf("parse wake Started timestamp: %w", err)
	}
	if current.Info == nil {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable metadata unavailable")
	}
	currentIdentity, ok := captureWakeFileIdentity(current.Info)
	if !ok || currentIdentity.Device == 0 || currentIdentity.Inode == 0 {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable identity unavailable")
	}

	staleByStarted := current.Info.ModTime().After(started.Add(wakeStartedTimestampUncertainty))
	comparisonEvidence := wakeBinaryEvidence{
		Available:      true,
		Current:        wakeBinaryFileEvidenceFromIdentity(currentIdentity),
		CurrentModTime: current.Info.ModTime().UnixNano(),
	}

	// Legacy locks have no serialized execution identity. Preserve the positive
	// timestamp warning, but never use this heuristic to claim a current image.
	if inspection.Lock.RunningImageEvidence == nil {
		return wakeBinaryStaleness{
			Stale:    staleByStarted,
			Method:   wakeBinaryComparisonStartedMTime,
			Evidence: comparisonEvidence,
		}, nil
	}

	recorded := *inspection.Lock.RunningImageEvidence
	if err := validateWakeImageEvidence(recorded); err != nil {
		return wakeBinaryStaleness{}, fmt.Errorf("validate recorded wake image evidence: %w", err)
	}
	if inspection.Lock.ImagePath != recorded.ExecutionPath ||
		inspection.Lock.ImageVersion != recorded.EmbeddedVersion {
		return wakeBinaryStaleness{}, fmt.Errorf("recorded wake image path or version disagrees with image evidence")
	}

	running, err := inspectDarwinWakeProcessImage(inspection.PID)
	if err != nil {
		if darwinWakeMappedImageGone(err) && inspection.IdentityConfirmed && inspection.Process.Running {
			imagePath := running.Path
			if imagePath == "" {
				imagePath = recorded.ExecutionPath
			}
			if imagePath == recorded.ExecutionPath {
				if _, pathErr := os.Lstat(imagePath); errors.Is(pathErr, fs.ErrNotExist) {
					return wakeBinaryStaleness{
						Stale:    true,
						Method:   wakeBinaryComparisonDarwinDeletedImage,
						Evidence: comparisonEvidence,
						Reason:   deletedWakeImageReason,
					}, nil
				}
			}
		}
		return wakeBinaryStaleness{}, err
	}
	restartStageAlias := sameDarwinWakeRestartStageAlias(recorded, running)
	if !sameDarwinWakeImagePath(recorded.ExecutionPath, running.Path) && !restartStageAlias {
		return wakeBinaryStaleness{}, fmt.Errorf("live wake image path disagrees with recorded image evidence")
	}
	comparisonEvidence.Running = wakeBinaryFileEvidenceFromIdentity(running.Identity)
	if err := confirmResolvedDarwinWakeBinary(current, currentIdentity); err != nil {
		return wakeBinaryStaleness{}, err
	}

	if running.Identity.Device != currentIdentity.Device || running.Identity.Inode != currentIdentity.Inode {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonDarwinProcessImage,
			Evidence: comparisonEvidence,
		}, nil
	}
	// The restart-stage hardlink changes the shared inode's ctime, while
	// proc_pidinfo may retain the mapped vnode's earlier snapshot. Only that
	// validated owned alias gets a ctime exception; every ordinary pathname
	// mismatch remains ambiguous.
	if !restartStageAlias &&
		(running.Identity.CTimeSec != currentIdentity.CTimeSec ||
			running.Identity.CTimeNsec != currentIdentity.CTimeNsec) {
		return wakeBinaryStaleness{}, fmt.Errorf("current wake image changed during comparison")
	}
	if running.Size != current.Info.Size() {
		return wakeBinaryStaleness{}, fmt.Errorf("current wake image changed during comparison")
	}
	if staleByStarted {
		// Started has second precision. A strictly newer current file remains a
		// conservative different-image proof even when its inode was preserved.
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonStartedMTime,
			Evidence: comparisonEvidence,
		}, nil
	}
	return wakeBinaryStaleness{
		Method:   wakeBinaryComparisonDarwinProcessImage,
		Evidence: comparisonEvidence,
	}, nil
}

func sameDarwinWakeImagePath(recorded, running string) bool {
	if recorded == running {
		return true
	}
	resolvedRecorded, recordedErr := filepath.EvalSymlinks(recorded)
	resolvedRunning, runningErr := filepath.EvalSymlinks(running)
	return recordedErr == nil && runningErr == nil &&
		filepath.Clean(resolvedRecorded) == filepath.Clean(resolvedRunning)
}

func sameDarwinWakeRestartStageAlias(
	recorded wakeImageEvidenceV1,
	running darwinMappedWakeImage,
) bool {
	if !validPreviousWakeRestartBoundImagePlatform(recorded) ||
		running.Identity.Device != recorded.Device ||
		running.Identity.Inode != recorded.Inode ||
		running.Size != recorded.Size {
		return false
	}
	info, err := os.Lstat(recorded.ExecutionPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() != recorded.Size {
		return false
	}
	identity, ok := captureWakeFileIdentity(info)
	return ok && identity.Device == recorded.Device && identity.Inode == recorded.Inode
}

type darwinWakeProcessImage = darwinMappedWakeImage

var inspectDarwinWakeProcessImage = inspectDarwinWakeMappedImage

func confirmResolvedDarwinWakeBinary(current resolvedWakeBinary, expected wakeFileIdentity) error {
	if !filepath.IsAbs(current.Path) || filepath.Clean(current.Path) != current.Path {
		return fmt.Errorf("current amq executable path is not canonical and absolute")
	}
	confirmed, err := os.Stat(current.Path)
	if err != nil {
		return fmt.Errorf("re-stat current amq executable: %w", err)
	}
	if !confirmed.Mode().IsRegular() {
		return fmt.Errorf("current amq executable is no longer a regular file")
	}
	identity, ok := captureWakeFileIdentity(confirmed)
	if !ok || identity != expected || confirmed.Size() != current.Info.Size() {
		return fmt.Errorf("current amq executable changed during comparison")
	}
	return nil
}

func wakeBinaryFileEvidenceFromIdentity(identity wakeFileIdentity) wakeBinaryFileEvidence {
	return wakeBinaryFileEvidence(identity)
}
