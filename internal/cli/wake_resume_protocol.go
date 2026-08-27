package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

const (
	wakeResumeSchemaV2   = 2
	wakeResumeSignalUSR1 = "SIGUSR1"
)

type wakeImageEvidenceV1 = selfupgrade.ImageEvidence

const (
	wakeImageEvidenceSchemaV1                 = selfupgrade.ImageEvidenceSchemaV1
	wakeImageMethodFDExec                     = selfupgrade.ImageMethodFDExec
	wakeImageMethodPathnameObserved           = selfupgrade.ImageMethodPathnameObserved
	wakeImageMethodPathnameExecObserved       = selfupgrade.ImageMethodPathnameExecObserved
	wakeImageMethodPathnameExecVerified       = selfupgrade.ImageMethodPathnameExecVerified
	wakeImageMethodPathnameExecVerifiedLegacy = selfupgrade.ImageMethodPathnameExecVerifiedLegacy
)

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
	return selfupgrade.ValidateImageEvidence(evidence)
}

func validateWakeImageEvidenceForPlatform(evidence wakeImageEvidenceV1, platform string) error {
	return selfupgrade.ValidateImageEvidenceForPlatform(evidence, platform)
}

func wakeImageMethodIsDarwinExecObserved(method string) bool {
	return selfupgrade.IsDarwinExecObserved(method)
}

func validWakeImageSHA256(value string) bool {
	return selfupgrade.ValidSHA256(value)
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
