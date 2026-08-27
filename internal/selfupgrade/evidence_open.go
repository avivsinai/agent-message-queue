package selfupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

var ErrImageChangedWhileHashing = errors.New("wake image changed while hashing")

func CaptureImageEvidenceFromOpenFile(
	file *os.File,
	executionPath, embeddedVersion, method string,
) (ImageEvidence, error) {
	return CaptureImageEvidenceFromOpenFileWithMutator(file, executionPath, embeddedVersion, method, nil)
}

func CaptureImageEvidenceFromOpenFileWithMutator(
	file *os.File,
	executionPath, embeddedVersion, method string,
	mutator func(),
) (ImageEvidence, error) {
	if file == nil {
		return ImageEvidence{}, fmt.Errorf("wake image file is missing")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ImageEvidence{}, fmt.Errorf("seek wake image: %w", err)
	}
	before, err := file.Stat()
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("stat opened wake image: %w", err)
	}
	if !before.Mode().IsRegular() {
		return ImageEvidence{}, fmt.Errorf("wake image must be a regular file")
	}
	if before.Mode().Perm()&0o111 == 0 {
		return ImageEvidence{}, fmt.Errorf("wake image is not executable")
	}
	if err := validateImagePathOwnership("wake image", executionPath, before); err != nil {
		return ImageEvidence{}, err
	}
	identity, ok := captureImageFileIdentity(before)
	if !ok {
		return ImageEvidence{}, fmt.Errorf("capture wake image identity")
	}

	hasher := sha256.New()
	if mutator != nil {
		mutator()
	}
	if _, err := io.Copy(hasher, file); err != nil {
		return ImageEvidence{}, fmt.Errorf("digest wake image: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("re-stat wake image: %w", err)
	}
	if !sameImageFileIdentity(before, after) || before.Size() != after.Size() {
		return ImageEvidence{}, ErrImageChangedWhileHashing
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ImageEvidence{}, fmt.Errorf("rewind wake image: %w", err)
	}

	evidence := ImageEvidence{
		Schema:          ImageEvidenceSchemaV1,
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
	if err := ValidateImageEvidence(evidence); err != nil {
		return ImageEvidence{}, err
	}
	return evidence, nil
}
