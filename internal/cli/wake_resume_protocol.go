package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeResumeSchemaV2        = 2
	wakeImageEvidenceSchemaV1 = 1

	wakeImageMethodFDExec                     = "fd_exec"
	wakeImageMethodPathnameObserved           = "pathname_observed"
	wakeImageMethodPathnameExecObserved       = "pathname_exec_observed"
	wakeImageMethodPathnameExecVerifiedLegacy = "pathname_execve_verified"
	wakeImageMethodPathnameExecVerified       = wakeImageMethodPathnameExecVerifiedLegacy
	wakeResumeSignalUSR1                      = "SIGUSR1"
)

// wakeImageEvidenceV1 records image metadata and its authority method.
// pathname_observed is diagnostic on every platform. A resumed Darwin wake
// publishes the private hardlink path observed by the successor after exec;
// Darwin cannot atomically bind exec to an already-open descriptor. A resumed
// Linux wake publishes evidence from its bound FD and verifies the running
// image through /proc/self/exe before rotating generation.
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

var errWakeResumeControlEndpointUnsupported = errors.New("wake resume control endpoint is unsupported")

func validateWakeResumeAdvertisement(lock wakeLock, expectedRoot, expectedAgent string) error {
	return validateWakeResumeAdvertisementWithContext(
		lock,
		expectedRoot,
		expectedAgent,
		runtime.GOOS,
		wakeControlSocketPath(expectedRoot, expectedAgent, lock.Generation),
	)
}

func validateWakeResumeAdvertisementWithContext(
	lock wakeLock,
	expectedRoot string,
	expectedAgent string,
	platform string,
	expectedControlSocket string,
) error {
	if strings.TrimSpace(expectedRoot) == "" {
		return fmt.Errorf("trusted wake resume root is empty")
	}
	if err := fsq.ValidateHandle(expectedAgent); err != nil {
		return fmt.Errorf("trusted wake resume agent is invalid: %w", err)
	}
	expectedRoot = canonicalWakeRoot(expectedRoot)
	if lock.Root != expectedRoot {
		return fmt.Errorf("wake resume root does not match the trusted root")
	}
	if err := fsq.ValidateHandle(lock.Agent); err != nil {
		return fmt.Errorf("wake resume agent is invalid: %w", err)
	}
	if lock.Agent != expectedAgent {
		return fmt.Errorf("wake resume agent does not match the trusted agent")
	}
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
	if lock.ResumeSignal != "" && lock.ResumeSignal != wakeResumeSignalUSR1 {
		return fmt.Errorf("wake resume signal is unsupported")
	}
	if lock.ResumeSignal == "" && strings.TrimSpace(lock.ControlSocket) == "" {
		return fmt.Errorf("wake resume control endpoint is missing")
	}
	if lock.ResumeSignal == "" && lock.ControlSocket != "" && expectedControlSocket != "" && lock.ControlSocket != expectedControlSocket {
		return fmt.Errorf("wake control endpoint does not match the exact root, agent, and generation")
	}
	if lock.SourceGeneration != "" || lock.SourceFloorDigest != "" {
		return fmt.Errorf("wake repair lineage is not resumable")
	}
	if lock.RunningImageEvidence == nil {
		return fmt.Errorf("wake running image evidence is missing")
	}
	if err := validateWakeImageEvidenceForPlatform(*lock.RunningImageEvidence, platform); err != nil {
		return err
	}
	// Resumed wakes publish execution-bound evidence. Ordinary Darwin wakes may
	// still publish pathname_observed evidence for diagnostics only.
	if lock.ImagePath != lock.RunningImageEvidence.ExecutionPath {
		return fmt.Errorf("wake image path does not match running image evidence")
	}
	if lock.ImageVersion != lock.RunningImageEvidence.EmbeddedVersion {
		return fmt.Errorf("wake image version does not match running image evidence")
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
	return validateWakeImageEvidenceForPlatform(evidence, runtime.GOOS)
}

func validateWakeImageEvidenceForPlatform(evidence wakeImageEvidenceV1, platform string) error {
	if evidence.Schema != wakeImageEvidenceSchemaV1 {
		return fmt.Errorf("wake image evidence schema %d unsupported", evidence.Schema)
	}
	if evidence.Platform != platform {
		return fmt.Errorf("wake image platform %q does not match %q", evidence.Platform, platform)
	}
	if platform == "darwin" {
		if evidence.Method != wakeImageMethodPathnameObserved &&
			!wakeImageMethodIsDarwinExecObserved(evidence.Method) {
			return fmt.Errorf("wake image method %q does not match a Darwin pathname evidence method", evidence.Method)
		}
	} else if platform == "linux" {
		if evidence.Method != wakeImageMethodPathnameObserved &&
			evidence.Method != wakeImageMethodFDExec {
			return fmt.Errorf("wake image method %q does not match a Linux image evidence method", evidence.Method)
		}
	} else if evidence.Method != wakeImageMethodFDExec {
		return fmt.Errorf("wake image method %q does not match platform method %q", evidence.Method, wakeImageMethodFDExec)
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
	if !validWakeImageSHA256(evidence.SHA256) {
		return fmt.Errorf("wake image sha256 is malformed")
	}
	version := strings.TrimSpace(evidence.EmbeddedVersion)
	if version == "" || strings.ContainsRune(version, 0) || version != evidence.EmbeddedVersion {
		return fmt.Errorf("wake image embedded version is missing or non-canonical")
	}
	return nil
}

func wakeImageMethodIsDarwinExecObserved(method string) bool {
	return method == wakeImageMethodPathnameExecObserved ||
		method == wakeImageMethodPathnameExecVerifiedLegacy
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
	case wakeInjectModeRaw, wakeInjectModePaste, wakeInjectModeNone:
		return true
	default:
		return false
	}
}
