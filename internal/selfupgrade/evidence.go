package selfupgrade

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	ImageEvidenceSchemaV1 = 1

	ImageMethodFDExec                     = "fd_exec"
	ImageMethodPathnameObserved           = "pathname_observed"
	ImageMethodPathnameExecObserved       = "pathname_exec_observed"
	ImageMethodPathnameExecVerifiedLegacy = "pathname_execve_verified"
	ImageMethodPathnameExecVerified       = ImageMethodPathnameExecVerifiedLegacy
)

// ImageEvidence records image metadata and the authority method used to
// observe it. A pathname observation is diagnostic; fd_exec and the Darwin
// pathname exec methods describe execution-bound evidence.
type ImageEvidence struct {
	Schema          int    `json:"schema"`
	Platform        string `json:"platform"`
	Method          string `json:"method"`
	ExecutionPath   string `json:"execution_path"`
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Size            int64  `json:"size"`
	CTimeNS         int64  `json:"ctime_ns"`
	SHA256          string `json:"sha256"`
	EmbeddedVersion string `json:"embedded_version"`
}

func ValidateImageEvidence(evidence ImageEvidence) error {
	return ValidateImageEvidenceForPlatform(evidence, runtime.GOOS)
}

func ValidateImageEvidenceForPlatform(evidence ImageEvidence, platform string) error {
	if evidence.Schema != ImageEvidenceSchemaV1 {
		return fmt.Errorf("wake image evidence schema %d unsupported", evidence.Schema)
	}
	if evidence.Platform != platform {
		return fmt.Errorf("wake image platform %q does not match %q", evidence.Platform, platform)
	}
	if platform == "darwin" {
		if evidence.Method != ImageMethodPathnameObserved &&
			!IsDarwinExecObserved(evidence.Method) {
			return fmt.Errorf("wake image method %q does not match a Darwin pathname evidence method", evidence.Method)
		}
	} else if platform == "linux" {
		if evidence.Method != ImageMethodPathnameObserved &&
			evidence.Method != ImageMethodFDExec {
			return fmt.Errorf("wake image method %q does not match a Linux image evidence method", evidence.Method)
		}
	} else if evidence.Method != ImageMethodFDExec {
		return fmt.Errorf("wake image method %q does not match platform method %q", evidence.Method, ImageMethodFDExec)
	}
	path := strings.TrimSpace(evidence.ExecutionPath)
	if path == "" || path != evidence.ExecutionPath || strings.ContainsRune(path, 0) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("wake image execution path must be a canonical absolute path")
	}
	if evidence.Device == 0 {
		return fmt.Errorf("wake image device is missing")
	}
	if evidence.Inode == 0 {
		return fmt.Errorf("wake image inode is missing")
	}
	if evidence.Size <= 0 {
		return fmt.Errorf("wake image size must be positive")
	}
	if evidence.CTimeNS <= 0 {
		return fmt.Errorf("wake image ctime is missing")
	}
	if !ValidSHA256(evidence.SHA256) {
		return fmt.Errorf("wake image sha256 is malformed")
	}
	version := strings.TrimSpace(evidence.EmbeddedVersion)
	if version == "" || strings.ContainsRune(version, 0) || version != evidence.EmbeddedVersion {
		return fmt.Errorf("wake image embedded version is missing or non-canonical")
	}
	return nil
}

func IsDarwinExecObserved(method string) bool {
	return method == ImageMethodPathnameExecObserved ||
		method == ImageMethodPathnameExecVerifiedLegacy
}

func ValidSHA256(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	digest := value[len(prefix):]
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32
}

func CaptureImageEvidence(path, embeddedVersion string) (ImageEvidence, error) {
	return CaptureImageEvidenceWithMutator(path, embeddedVersion, nil)
}

// CaptureImageEvidenceWithEmbeddedVersion reads the candidate version and
// captures its evidence from the same opened file.
func CaptureImageEvidenceWithEmbeddedVersion(path string) (ImageEvidence, error) {
	path = strings.TrimSpace(path)
	file, before, err := openImageEvidenceFile(path)
	if err != nil {
		return ImageEvidence{}, err
	}
	defer func() { _ = file.Close() }()

	version, err := ReadEmbeddedVersionFromOpenFile(file)
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("read wake image version: %w", err)
	}
	evidence, err := CaptureImageEvidenceFromOpenFile(
		file,
		path,
		version,
		ImageMethodPathnameObserved,
	)
	if err != nil {
		return ImageEvidence{}, err
	}
	confirmed, err := file.Stat()
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("re-stat wake image: %w", err)
	}
	if !sameImageFileIdentity(before, confirmed) || before.Size() != confirmed.Size() {
		return ImageEvidence{}, ErrImageChangedWhileHashing
	}
	return evidence, nil
}

// CaptureImageEvidenceWithMutator exists for race tests that mutate the file
// between the before-hash and after-hash observations.
func CaptureImageEvidenceWithMutator(path, embeddedVersion string, mutator func()) (ImageEvidence, error) {
	path = strings.TrimSpace(path)
	file, before, err := openImageEvidenceFile(path)
	if err != nil {
		return ImageEvidence{}, err
	}
	defer func() { _ = file.Close() }()

	evidence, err := CaptureImageEvidenceFromOpenFileWithMutator(
		file,
		path,
		embeddedVersion,
		ImageMethodPathnameObserved,
		mutator,
	)
	if err != nil {
		return ImageEvidence{}, err
	}
	confirmed, err := file.Stat()
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("re-stat wake image: %w", err)
	}
	if !sameImageFileIdentity(before, confirmed) || before.Size() != confirmed.Size() {
		return ImageEvidence{}, ErrImageChangedWhileHashing
	}
	return evidence, nil
}

func openImageEvidenceFile(path string) (*os.File, os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, fmt.Errorf("wake image path must be a canonical absolute path")
	}
	lstat, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake image: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("wake image must not be a symlink")
	}
	file, err := openImageMetadataFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open wake image: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	before, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat opened wake image: %w", err)
	}
	if !sameImageFileIdentity(lstat, before) {
		return nil, nil, fmt.Errorf("wake image changed while opening")
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("wake image must be a regular file")
	}
	if before.Mode().Perm()&0o111 == 0 {
		return nil, nil, fmt.Errorf("wake image is not executable")
	}
	if err := validateImagePathOwnership("wake image", path, before); err != nil {
		return nil, nil, err
	}
	closeOnError = false
	return file, before, nil
}
