//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const wakeStartedTimestampUncertainty = time.Second

const deletedWakeImageReason = "wake is running a deleted image; restart it"

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
		if errors.Is(err, fs.ErrNotExist) && inspection.IdentityConfirmed && inspection.Process.Running {
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
	if recorded.ExecutionPath != running.Path {
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
	if running.Identity != currentIdentity || running.Size != current.Info.Size() {
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
