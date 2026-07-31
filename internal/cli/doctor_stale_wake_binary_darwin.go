//go:build darwin

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const wakeStartedTimestampUncertainty = time.Second

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
		return wakeBinaryStaleness{}, err
	}
	if !darwinRecordedImageMatches(recorded, running) {
		return wakeBinaryStaleness{}, fmt.Errorf("live wake image disagrees with recorded image evidence")
	}
	comparisonEvidence.Running = wakeBinaryFileEvidenceFromIdentity(running.Identity)

	if running.Identity.Device != currentIdentity.Device || running.Identity.Inode != currentIdentity.Inode {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonDarwinProcessImage,
			Evidence: comparisonEvidence,
		}, nil
	}
	if running.Identity != currentIdentity || running.Info.Size() != current.Info.Size() {
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

type darwinWakeProcessImage struct {
	Path     string
	Info     os.FileInfo
	Identity wakeFileIdentity
	SHA256   string
}

func inspectDarwinWakeProcessImage(pid int) (darwinWakeProcessImage, error) {
	path, err := readDarwinProcessExecutablePath(pid)
	if err != nil {
		return darwinWakeProcessImage{}, fmt.Errorf("resolve wake process executable: %w", err)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable path is not canonical and absolute")
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		return darwinWakeProcessImage{}, fmt.Errorf("stat wake process executable path: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable path is not a regular non-symlink file")
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return darwinWakeProcessImage{}, fmt.Errorf("open wake process executable: %w", err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil {
		return darwinWakeProcessImage{}, fmt.Errorf("stat opened wake process executable: %w", err)
	}
	if !sameWakeFileIdentity(pathInfo, openedInfo) || pathInfo.Size() != openedInfo.Size() {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable changed while opening")
	}
	identity, ok := captureWakeFileIdentity(openedInfo)
	if !ok || identity.Device == 0 || identity.Inode == 0 {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable identity unavailable")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return darwinWakeProcessImage{}, fmt.Errorf("digest wake process executable: %w", err)
	}
	hashedInfo, err := file.Stat()
	if err != nil || !sameWakeFileIdentity(openedInfo, hashedInfo) || openedInfo.Size() != hashedInfo.Size() {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable changed while hashing")
	}

	confirmedPath, err := readDarwinProcessExecutablePath(pid)
	if err != nil || confirmedPath != path {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable path changed during inspection")
	}
	confirmedInfo, err := os.Lstat(path)
	if err != nil || !sameWakeFileIdentity(hashedInfo, confirmedInfo) || hashedInfo.Size() != confirmedInfo.Size() {
		return darwinWakeProcessImage{}, fmt.Errorf("wake process executable changed during inspection")
	}
	return darwinWakeProcessImage{
		Path:     path,
		Info:     hashedInfo,
		Identity: identity,
		SHA256:   "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func darwinRecordedImageMatches(
	recorded wakeImageEvidenceV1,
	running darwinWakeProcessImage,
) bool {
	return recorded.ExecutionPath == running.Path &&
		recorded.Device == running.Identity.Device &&
		recorded.Inode == running.Identity.Inode &&
		recorded.Size == running.Info.Size() &&
		recorded.CTimeNS == running.Identity.CTimeSec*1_000_000_000+running.Identity.CTimeNsec &&
		recorded.SHA256 == running.SHA256
}

func wakeBinaryFileEvidenceFromIdentity(identity wakeFileIdentity) wakeBinaryFileEvidence {
	return wakeBinaryFileEvidence(identity)
}
