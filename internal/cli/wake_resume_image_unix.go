//go:build darwin || linux

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var captureCurrentWakeImageEvidence = captureCurrentWakeImageEvidenceDefault

func captureCurrentWakeImageEvidenceDefault() (wakeImageEvidenceV1, error) {
	path, err := os.Executable()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve current wake executable: %w", err)
	}
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return wakeImageEvidenceV1{}, fmt.Errorf("current wake executable path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve current wake executable symlinks: %w", err)
	}
	// Resolving the process-reported pathname does not bind this open to the
	// image actually executing. Keep it diagnostic on every platform.
	return captureWakeImageEvidence(filepath.Clean(resolved), strings.TrimSpace(cliVersion))
}

func captureWakeImageEvidence(path, embeddedVersion string) (wakeImageEvidenceV1, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image path must be a canonical absolute path")
	}
	lstat, err := os.Lstat(path)
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("stat wake image: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image must not be a symlink")
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("open wake image: %w", err)
	}
	defer func() { _ = file.Close() }()

	before, err := file.Stat()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("stat opened wake image: %w", err)
	}
	if !os.SameFile(lstat, before) {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image changed while opening")
	}
	if !before.Mode().IsRegular() {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image must be a regular file")
	}
	if before.Mode().Perm()&0o111 == 0 {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image is not executable")
	}
	if err := validateWakeTargetPathOwnership("wake image", path, before); err != nil {
		return wakeImageEvidenceV1{}, err
	}
	evidence, err := captureWakeImageEvidenceFromOpenFile(
		file,
		path,
		embeddedVersion,
		wakeImageMethodPathnameObserved,
	)
	if err != nil {
		return wakeImageEvidenceV1{}, err
	}
	confirmed, err := file.Stat()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("re-stat wake image: %w", err)
	}
	if !sameWakeImageStableAcrossHash(before, confirmed) {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image changed while hashing")
	}
	return evidence, nil
}

func captureWakeImageEvidenceFromOpenFile(
	file *os.File,
	executionPath, embeddedVersion, method string,
) (wakeImageEvidenceV1, error) {
	if file == nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image file is missing")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("seek wake image: %w", err)
	}
	before, err := file.Stat()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("stat opened wake image: %w", err)
	}
	if !before.Mode().IsRegular() {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image must be a regular file")
	}
	if before.Mode().Perm()&0o111 == 0 {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image is not executable")
	}
	if err := validateWakeTargetPathOwnership("wake image", executionPath, before); err != nil {
		return wakeImageEvidenceV1{}, err
	}
	identity, ok := captureWakeFileIdentity(before)
	if !ok {
		return wakeImageEvidenceV1{}, fmt.Errorf("capture wake image identity")
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("digest wake image: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("re-stat wake image: %w", err)
	}
	if !sameWakeImageStableAcrossHash(before, after) {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake image changed while hashing")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("rewind wake image: %w", err)
	}

	evidence := wakeImageEvidenceV1{
		Schema:          wakeImageEvidenceSchemaV1,
		Platform:        runtime.GOOS,
		Method:          method,
		ExecutionPath:   executionPath,
		Device:          identity.Device,
		Inode:           identity.Inode,
		Size:            after.Size(),
		CTimeNS:         identity.CTimeSec*1_000_000_000 + identity.CTimeNsec,
		SHA256:          "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
		EmbeddedVersion: embeddedVersion,
	}
	if err := validateWakeImageEvidence(evidence); err != nil {
		return wakeImageEvidenceV1{}, err
	}
	return evidence, nil
}
