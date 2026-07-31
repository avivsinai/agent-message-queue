package cli

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	wakeResumeSchemaV2        = 2
	wakeImageEvidenceSchemaV1 = 1

	wakeImageMethodFDExec               = "fd_exec"
	wakeImageMethodPathnameExecVerified = "pathname_execve_verified"
)

// wakeImageEvidenceV1 is serialized execution evidence, not a path/version
// hint. Later waves use Method to preserve the honest platform asymmetry:
// Linux retains an fd authority; Darwin immediately re-verifies the resolved
// versioned pathname before execve.
type wakeImageEvidenceV1 struct {
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

func validateWakeResumeAdvertisement(lock wakeLock) error {
	if lock.ResumeSchema != wakeResumeSchemaV2 {
		if lock.ResumeSchema == 0 {
			return fmt.Errorf("wake resume schema is missing")
		}
		return fmt.Errorf("wake resume schema %d unsupported", lock.ResumeSchema)
	}
	if lock.ResumeOwner == nil {
		return fmt.Errorf("wake resume owner is missing")
	}
	if err := validateAuthoritativeWakeOwner(*lock.ResumeOwner); err != nil {
		return fmt.Errorf("wake resume owner is invalid: %w", err)
	}
	if strings.TrimSpace(lock.Generation) == "" {
		return fmt.Errorf("wake resume generation is missing")
	}
	if err := validateAuthoritativeWakeProcessIdentity(lock); err != nil {
		return fmt.Errorf("wake resume process identity is invalid: %w", err)
	}
	if strings.TrimSpace(lock.ControlSocket) == "" {
		return fmt.Errorf("wake resume control endpoint is missing")
	}
	if lock.SourceGeneration != "" || lock.SourceFloorDigest != "" {
		return fmt.Errorf("wake repair lineage is not resumable")
	}
	if lock.RunningImageEvidence == nil {
		return fmt.Errorf("wake running image evidence is missing")
	}
	if err := validateWakeImageEvidence(*lock.RunningImageEvidence); err != nil {
		return err
	}
	if lock.WakeMode == wakeOwnerWakeMode {
		if lock.OwnerSchema != wakeOwnerLockSchema || lock.Owner == nil {
			return fmt.Errorf("authoritative wake owner is incomplete")
		}
		if !sameWakeOwner(lock.Owner, lock.ResumeOwner) {
			return fmt.Errorf("wake resume owner does not match authoritative wake owner")
		}
	}
	return nil
}

func validateWakeImageEvidence(evidence wakeImageEvidenceV1) error {
	if evidence.Schema != wakeImageEvidenceSchemaV1 {
		return fmt.Errorf("wake image evidence schema %d unsupported", evidence.Schema)
	}
	if evidence.Platform != runtime.GOOS {
		return fmt.Errorf("wake image platform %q does not match %q", evidence.Platform, runtime.GOOS)
	}
	expectedMethod := wakeImageMethodFDExec
	if runtime.GOOS == "darwin" {
		expectedMethod = wakeImageMethodPathnameExecVerified
	}
	if evidence.Method != expectedMethod {
		return fmt.Errorf("wake image method %q does not match platform method %q", evidence.Method, expectedMethod)
	}
	path := strings.TrimSpace(evidence.ExecutionPath)
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
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
	if !validWakeImageSHA256(evidence.SHA256) {
		return fmt.Errorf("wake image sha256 is malformed")
	}
	version := strings.TrimSpace(evidence.EmbeddedVersion)
	if version == "" || strings.ContainsRune(version, 0) || version != evidence.EmbeddedVersion {
		return fmt.Errorf("wake image embedded version is missing or non-canonical")
	}
	return nil
}

func validWakeImageSHA256(value string) bool {
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

func wakeResumeStartupEligible(
	owner *wakeOwner,
	repairLineage bool,
	injectCmd string,
	interruptKey string,
	wakeMode string,
) bool {
	if owner == nil || validateAuthoritativeWakeOwner(*owner) != nil || repairLineage ||
		strings.TrimSpace(injectCmd) != "" || interruptKey != "" {
		return false
	}
	switch wakeMode {
	case wakeInjectModeRaw, wakeInjectModePaste, wakeInjectModeNone, wakeTargetInjectVia:
		return true
	default:
		return false
	}
}
