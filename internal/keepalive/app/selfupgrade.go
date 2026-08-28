package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

const (
	selfUpgradeStateSchema      = 2
	selfUpgradeStateSchemaV1    = 1
	selfUpgradeStateFileName    = ".selfupgrade.json"
	selfUpgradeAttemptsFileName = ".selfupgrade.attempts"
	selfUpgradeStateLockName    = ".selfupgrade.lock"
	selfUpgradeActionUnchanged  = "unchanged"
	selfUpgradeActionRefused    = "refused"
	selfUpgradeActionExec       = "exec"
	selfUpgradeAttemptsSchema   = 1
)

var (
	errSelfUpgradeAttemptTimestampUncertain = errors.New("replacement attempt timestamp is uncertain")
	errSelfUpgradeNoMatchingAttempt         = errors.New("no matching self-upgrade attempt")
	errSelfUpgradeAttemptSettled            = errors.New("replacement attempt is already settled")
	errSelfUpgradeCandidateRefused          = errors.New("candidate is already refused")
)

var (
	selfUpgradeExecutable       = os.Executable
	selfUpgradeEvalSymlinks     = filepath.EvalSymlinks
	selfUpgradeCaptureImage     = selfupgrade.CaptureImageEvidence
	selfUpgradeCaptureCandidate = selfupgrade.CaptureImageEvidenceWithEmbeddedVersion
	selfUpgradeExecImage        = selfupgrade.ExecImage
	selfUpgradeGeneration       = newSelfUpgradeGeneration
	selfUpgradeStateFileInfo    = os.Lstat
	selfUpgradeStateReadFile    = os.ReadFile
	selfUpgradeStateCreateTemp  = os.CreateTemp
	selfUpgradeStateRename      = os.Rename
	selfUpgradeStateRemove      = os.Remove
	selfUpgradeStateWrite       = io.WriteString
	selfUpgradeSyncDir          = syncSelfUpgradeDir
	selfUpgradeAcquireLock      = acquireSelfUpgradeStateLock
	selfUpgradeNow              = time.Now
)

type selfUpgradeController struct {
	enabled              bool
	eligible             bool
	reason               string
	locator              string
	incumbent            selfupgrade.ImageEvidence
	generation           string
	statePath            string
	refused              []selfupgrade.RefusedCandidate
	attempts             []selfupgrade.Attempt
	startupRefusalReason string
	statePublished       bool
	ledgerVersion        string
	lastObservation      *selfUpgradeObservation
}

type selfUpgradeObservation struct {
	evidence      selfupgrade.ImageEvidence
	action        string
	ledgerVersion string
}

type selfUpgradeAttemptRefusalError struct {
	attempt selfupgrade.Attempt
}

func (err selfUpgradeAttemptRefusalError) Error() string {
	return err.attempt.RefusalReason()
}

type selfUpgradeStateFile struct {
	Schema                      int                            `json:"schema"`
	Generation                  string                         `json:"generation"`
	IncumbentVersion            string                         `json:"incumbent_version"`
	IncumbentSHA256             string                         `json:"incumbent_sha256"`
	RefusedCandidates           []selfupgrade.RefusedCandidate `json:"refused_candidates,omitempty"`
	Attempts                    []selfupgrade.Attempt          `json:"attempts,omitempty"`
	Attempt                     *selfupgrade.Attempt           `json:"attempt,omitempty"`
	attemptsSidecarExists       bool
	attemptsSidecarSynchronized bool
}

type selfUpgradeAttemptsFile struct {
	Schema   int                   `json:"schema"`
	Attempts []selfupgrade.Attempt `json:"attempts,omitempty"`
}

func newSelfUpgradeController(registryPath, locator, version string, enabled bool) *selfUpgradeController {
	controller := &selfUpgradeController{enabled: enabled}
	if !enabled {
		controller.reason = "disabled by --no-self-upgrade"
		return controller
	}
	if !selfupgrade.ExecSupported() {
		controller.reason = fmt.Sprintf("self-upgrade is unsupported on %s", runtime.GOOS)
		return controller
	}
	if strings.TrimSpace(version) == "" || strings.TrimSpace(version) != version {
		controller.reason = "running image version is unavailable"
		return controller
	}
	generation, err := selfUpgradeGeneration()
	if err != nil {
		controller.reason = "self-upgrade generation is unavailable"
		return controller
	}
	controller.generation = generation
	path, err := canonicalSelfUpgradePath(locator)
	if err != nil {
		controller.reason = err.Error()
		return controller
	}
	controller.locator = path
	registryPath, err = canonicalSelfUpgradePath(registryPath)
	if err != nil {
		controller.reason = fmt.Sprintf("self-upgrade state path is unavailable: %v", err)
		return controller
	}
	controller.statePath = filepath.Join(filepath.Dir(registryPath), selfUpgradeStateFileName)

	runningPath, err := selfUpgradeExecutable()
	if err != nil {
		controller.reason = fmt.Sprintf("resolve running image: %v", err)
		return controller
	}
	runningPath, err = resolvedSelfUpgradePath(runningPath)
	if err != nil {
		controller.reason = fmt.Sprintf("resolve running image: %v", err)
		return controller
	}
	incumbent, err := selfUpgradeCaptureImage(runningPath, version)
	if err != nil {
		controller.reason = fmt.Sprintf("capture running image: %v", err)
		return controller
	}
	locatorImage, err := selfUpgradeCaptureImage(path, version)
	if err != nil {
		controller.reason = fmt.Sprintf("capture self-upgrade locator: %v", err)
		return controller
	}
	if !sameSelfUpgradeFileIdentity(incumbent, locatorImage) {
		controller.reason = "self-upgrade locator does not name the running image"
		return controller
	}
	controller.incumbent = incumbent
	if err := selfupgrade.CleanupStages(path); err != nil {
		controller.reason = fmt.Sprintf("clean up self-upgrade stages: %v", err)
		return controller
	}
	if err := loadSelfUpgradeState(controller); err != nil {
		controller.reason = fmt.Sprintf("self-upgrade refusal state is unavailable: %v", err)
		return controller
	}
	if controller.reason != "" {
		return controller
	}
	controller.eligible = true
	return controller
}

func canonicalSelfUpgradePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("self-upgrade path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve self-upgrade path: %w", err)
	}
	abs = filepath.Clean(abs)
	if !filepath.IsAbs(abs) || filepath.Clean(abs) != abs {
		return "", errors.New("self-upgrade path is not canonical")
	}
	return abs, nil
}

func selfUpgradeDefaultLocator() string {
	if len(os.Args) > 0 {
		argv0 := strings.TrimSpace(os.Args[0])
		if argv0 != "" {
			if resolved, err := exec.LookPath(argv0); err == nil {
				return resolved
			}
			return argv0
		}
	}
	return executablePath()
}

func resolvedSelfUpgradePath(path string) (string, error) {
	path, err := canonicalSelfUpgradePath(path)
	if err != nil {
		return "", err
	}
	resolved, err := selfUpgradeEvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = canonicalSelfUpgradePath(resolved)
	if err != nil {
		return "", fmt.Errorf("resolved self-upgrade path: %w", err)
	}
	return resolved, nil
}

func newSelfUpgradeGeneration() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%x", os.Getpid(), bytes[:]), nil
}

func loadSelfUpgradeState(controller *selfUpgradeController) error {
	state, exists, err := readSelfUpgradeStateFile(controller.statePath)
	if err != nil {
		return err
	}
	return controller.applySelfUpgradeState(state, exists, true)
}

func (controller *selfUpgradeController) applySelfUpgradeState(
	state selfUpgradeStateFile,
	exists bool,
	startup bool,
) error {
	if !exists {
		controller.attempts = nil
		controller.refused = nil
		controller.statePublished = false
		controller.ledgerVersion = selfUpgradeLedgerVersion(controller.generation, nil, nil)
		return nil
	}
	now := selfUpgradeNow()
	controller.attempts = selfupgrade.PruneExpiredAttempts(state.Attempts, now)
	if state.Generation == controller.generation {
		controller.refused = append([]selfupgrade.RefusedCandidate(nil), state.RefusedCandidates...)
	} else {
		controller.refused = nil
	}
	controller.ledgerVersion = selfUpgradeLedgerVersion(state.Generation, controller.refused, controller.attempts)
	controller.statePublished = state.Schema == selfUpgradeStateSchema &&
		state.Generation == controller.generation &&
		state.attemptsSidecarExists &&
		state.attemptsSidecarSynchronized &&
		len(controller.attempts) == len(state.Attempts)
	for _, attempt := range controller.attempts {
		if attempt.Status == selfupgrade.AttemptStatusAttempt && attempt.IsFutureUncertain(now) {
			controller.reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
			controller.eligible = false
		}
		if startup && controller.startupRefusalReason == "" &&
			attempt.Status == selfupgrade.AttemptStatusAttempt &&
			attempt.IsFresh(now) && attempt.Matches(controller.incumbent) {
			controller.startupRefusalReason = fmt.Sprintf(
				"refused unsettled self-upgrade image while the attempt is fresh relative to its recorded timestamp under the current wall clock (candidate=%s)",
				selfUpgradeEvidenceIdentityString(controller.incumbent),
			)
		}
	}
	return nil
}

func (controller *selfUpgradeController) refreshSelfUpgradeState() error {
	previousVersion := controller.ledgerVersion
	release, err := selfUpgradeAcquireLock(controller.statePath)
	if err != nil {
		return err
	}
	state, exists, readErr := readSelfUpgradeStateFile(controller.statePath)
	if readErr == nil {
		readErr = controller.applySelfUpgradeState(state, exists, false)
		if readErr == nil && controller.lastObservation != nil &&
			previousVersion != controller.ledgerVersion {
			controller.lastObservation = nil
		}
	}
	releaseErr := release()
	return errors.Join(readErr, releaseErr)
}

func selfUpgradeAttemptsPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), selfUpgradeAttemptsFileName)
}

func readSelfUpgradeAttemptsFile(statePath string) ([]selfupgrade.Attempt, bool, error) {
	path := selfUpgradeAttemptsPath(statePath)
	info, err := selfUpgradeStateFileInfo(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, true, errors.New("self-upgrade attempt state is not a private regular file")
	}
	raw, err := selfUpgradeStateReadFile(path)
	if err != nil {
		return nil, true, err
	}
	var state selfUpgradeAttemptsFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, true, err
	}
	if state.Schema != selfUpgradeAttemptsSchema {
		return nil, true, errors.New("self-upgrade attempt state schema is invalid")
	}
	if err := selfupgrade.ValidateAttempts(state.Attempts); err != nil {
		return nil, true, err
	}
	return state.Attempts, true, nil
}

func readSelfUpgradeStateFile(path string) (selfUpgradeStateFile, bool, error) {
	info, err := selfUpgradeStateFileInfo(path)
	if errors.Is(err, os.ErrNotExist) {
		_, attemptsExists, attemptsErr := readSelfUpgradeAttemptsFile(path)
		if attemptsErr != nil {
			return selfUpgradeStateFile{}, true, attemptsErr
		}
		if attemptsExists {
			return selfUpgradeStateFile{}, true, errors.New("self-upgrade attempt state exists without refusal state")
		}
		return selfUpgradeStateFile{}, false, nil
	}
	if err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return selfUpgradeStateFile{}, true, errors.New("self-upgrade refusal state is not a private regular file")
	}
	raw, err := selfUpgradeStateReadFile(path)
	if err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	var state selfUpgradeStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	if strings.TrimSpace(state.Generation) == "" {
		return selfUpgradeStateFile{}, true, errors.New("self-upgrade refusal state schema is invalid")
	}
	switch state.Schema {
	case selfUpgradeStateSchemaV1:
		if len(state.Attempts) != 0 {
			return selfUpgradeStateFile{}, true, errors.New("self-upgrade refusal state schema 1 contains a ledger")
		}
		if state.Attempt != nil {
			state.Attempts = []selfupgrade.Attempt{*state.Attempt}
		}
	case selfUpgradeStateSchema:
		if state.Attempt != nil {
			return selfUpgradeStateFile{}, true, errors.New("self-upgrade refusal state schema 2 contains a legacy attempt")
		}
	default:
		return selfUpgradeStateFile{}, true, errors.New("self-upgrade refusal state schema is invalid")
	}
	if err := validateSelfUpgradeRefusals(state.RefusedCandidates); err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	if err := selfupgrade.ValidateAttempts(state.Attempts); err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	primaryAttempts := append([]selfupgrade.Attempt(nil), state.Attempts...)
	attempts, attemptsExists, err := readSelfUpgradeAttemptsFile(path)
	if err != nil {
		return selfUpgradeStateFile{}, true, err
	}
	if attemptsExists {
		merged, err := selfupgrade.MergeAttempts(state.Attempts, attempts, selfUpgradeNow())
		if err != nil {
			return selfUpgradeStateFile{}, true, err
		}
		state.Attempts = nilIfEmptySelfUpgradeAttempts(merged)
	}
	state.attemptsSidecarExists = attemptsExists
	state.attemptsSidecarSynchronized = attemptsExists && reflect.DeepEqual(primaryAttempts, attempts)
	return state, true, nil
}

func validateSelfUpgradeRefusals(candidates []selfupgrade.RefusedCandidate) error {
	if len(candidates) > selfupgrade.RefusalLimit {
		return fmt.Errorf("self-upgrade refusal state exceeds the limit of %d", selfupgrade.RefusalLimit)
	}
	seen := make(map[selfupgrade.RefusedCandidate]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Platform != runtime.GOOS || candidate.Device == 0 || candidate.Inode == 0 ||
			candidate.Size <= 0 || !selfupgrade.ValidSHA256(candidate.SHA256) {
			return errors.New("self-upgrade refusal identity is invalid")
		}
		version := strings.TrimSpace(candidate.EmbeddedVersion)
		if version == "" || version != candidate.EmbeddedVersion || strings.ContainsRune(version, 0) {
			return errors.New("self-upgrade refusal version is invalid")
		}
		if _, exists := seen[candidate]; exists {
			return errors.New("self-upgrade refusal state contains a duplicate candidate")
		}
		seen[candidate] = struct{}{}
	}
	return nil
}

func validateSelfUpgradeAttemptTimes(attempts []selfupgrade.Attempt, now time.Time) error {
	for _, attempt := range attempts {
		if attempt.Status == selfupgrade.AttemptStatusAttempt && attempt.IsFutureUncertain(now) {
			return errSelfUpgradeAttemptTimestampUncertain
		}
	}
	return nil
}

func (controller *selfUpgradeController) ensureStatePublished() error {
	if controller.statePublished {
		return nil
	}
	if err := controller.saveState(); err != nil {
		controller.eligible = false
		controller.reason = fmt.Sprintf("publish self-upgrade state: %v", err)
		return err
	}
	controller.statePublished = true
	return nil
}

func (controller *selfUpgradeController) saveState() error {
	return controller.publishState(nil)
}

func (controller *selfUpgradeController) publishState(mutator func(*selfUpgradeStateFile) error) error {
	if controller == nil {
		return errors.New("self-upgrade controller is nil")
	}
	if controller.statePath == "" {
		return errors.New("self-upgrade state path is empty")
	}
	release, err := selfUpgradeAcquireLock(controller.statePath)
	if err != nil {
		return err
	}
	publishErr := controller.publishStateLocked(mutator)
	releaseErr := release()
	return errors.Join(publishErr, releaseErr)
}

func (controller *selfUpgradeController) publishStateLocked(mutator func(*selfUpgradeStateFile) error) error {
	current, exists, err := readSelfUpgradeStateFile(controller.statePath)
	if err != nil {
		return err
	}
	state, err := controller.mergeState(current, exists)
	if err != nil {
		return err
	}
	if mutator != nil {
		if err := mutator(&state); err != nil {
			return err
		}
	}
	state.Schema = selfUpgradeStateSchema
	state.Attempt = nil
	state.Attempts = nilIfEmptySelfUpgradeAttempts(state.Attempts)
	state.RefusedCandidates = nilIfEmptySelfUpgradeRefusals(state.RefusedCandidates)
	if err := validateSelfUpgradeRefusals(state.RefusedCandidates); err != nil {
		return err
	}
	if err := selfupgrade.ValidateAttempts(state.Attempts); err != nil {
		return err
	}
	if err := controller.writeState(state); err != nil {
		return err
	}
	controller.refused = append([]selfupgrade.RefusedCandidate(nil), state.RefusedCandidates...)
	controller.attempts = append([]selfupgrade.Attempt(nil), state.Attempts...)
	controller.statePublished = true
	controller.ledgerVersion = selfUpgradeLedgerVersion(state.Generation, state.RefusedCandidates, state.Attempts)
	return nil
}

func (controller *selfUpgradeController) mergeState(
	current selfUpgradeStateFile,
	exists bool,
) (selfUpgradeStateFile, error) {
	return controller.mergeStateAt(current, exists, selfUpgradeNow())
}

func (controller *selfUpgradeController) mergeStateAt(
	current selfUpgradeStateFile,
	exists bool,
	now time.Time,
) (selfUpgradeStateFile, error) {
	desired := selfUpgradeStateFile{
		Schema:            selfUpgradeStateSchema,
		Generation:        controller.generation,
		IncumbentVersion:  controller.incumbent.EmbeddedVersion,
		IncumbentSHA256:   controller.incumbent.SHA256,
		RefusedCandidates: append([]selfupgrade.RefusedCandidate(nil), controller.refused...),
		Attempts:          append([]selfupgrade.Attempt(nil), controller.attempts...),
	}
	if err := validateSelfUpgradeAttemptTimes(desired.Attempts, now); err != nil {
		return selfUpgradeStateFile{}, err
	}
	if !exists {
		return desired, nil
	}
	if err := validateSelfUpgradeAttemptTimes(current.Attempts, now); err != nil {
		return selfUpgradeStateFile{}, err
	}
	mergedAttempts, err := selfupgrade.MergeAttempts(current.Attempts, desired.Attempts, now)
	if err != nil {
		return selfUpgradeStateFile{}, err
	}
	desired.Attempts = mergedAttempts
	if current.Generation == controller.generation {
		desired.RefusedCandidates = mergeSelfUpgradeRefusals(
			current.RefusedCandidates,
			desired.RefusedCandidates,
		)
	}
	return desired, nil
}

func mergeSelfUpgradeRefusals(
	current, desired []selfupgrade.RefusedCandidate,
) []selfupgrade.RefusedCandidate {
	merged := make([]selfupgrade.RefusedCandidate, 0, len(current)+len(desired))
	for _, candidates := range [][]selfupgrade.RefusedCandidate{current, desired} {
		for _, candidate := range candidates {
			seen := false
			for _, existing := range merged {
				if existing == candidate {
					seen = true
					break
				}
			}
			if !seen {
				merged = append(merged, candidate)
			}
		}
	}
	if len(merged) > selfupgrade.RefusalLimit {
		merged = append([]selfupgrade.RefusedCandidate(nil), merged[len(merged)-selfupgrade.RefusalLimit:]...)
	}
	return merged
}

func nilIfEmptySelfUpgradeAttempts(attempts []selfupgrade.Attempt) []selfupgrade.Attempt {
	if len(attempts) == 0 {
		return nil
	}
	return attempts
}

func nilIfEmptySelfUpgradeRefusals(candidates []selfupgrade.RefusedCandidate) []selfupgrade.RefusedCandidate {
	if len(candidates) == 0 {
		return nil
	}
	return candidates
}

func selfUpgradeLedgerVersion(
	generation string,
	refused []selfupgrade.RefusedCandidate,
	attempts []selfupgrade.Attempt,
) string {
	data, _ := json.Marshal(struct {
		Generation string                         `json:"generation"`
		Refused    []selfupgrade.RefusedCandidate `json:"refused"`
		Attempts   []selfupgrade.Attempt          `json:"attempts"`
	}{
		Generation: generation,
		Refused:    refused,
		Attempts:   attempts,
	})
	digest := sha256.Sum256(data)
	return string(digest[:])
}

func (controller *selfUpgradeController) observe(
	evidence selfupgrade.ImageEvidence,
	action string,
) *selfUpgradeObservation {
	return &selfUpgradeObservation{
		evidence:      evidence,
		action:        action,
		ledgerVersion: controller.ledgerVersion,
	}
}

func (controller *selfUpgradeController) writeState(state selfUpgradeStateFile) error {
	if controller.statePath == "" {
		return errors.New("self-upgrade state path is empty")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	attemptsData, err := json.MarshalIndent(selfUpgradeAttemptsFile{
		Schema:   selfUpgradeAttemptsSchema,
		Attempts: nilIfEmptySelfUpgradeAttempts(state.Attempts),
	}, "", "  ")
	if err != nil {
		return err
	}
	attemptsData = append(attemptsData, '\n')
	if err := writeSelfUpgradeJSONFile(selfUpgradeAttemptsPath(controller.statePath), attemptsData); err != nil {
		return fmt.Errorf("write self-upgrade attempt state: %w", err)
	}
	if err := writeSelfUpgradeJSONFile(controller.statePath, data); err != nil {
		return err
	}
	installed, exists, err := readSelfUpgradeStateFile(controller.statePath)
	if err != nil {
		return fmt.Errorf("read back self-upgrade state: %w", err)
	}
	if !exists || installed.Schema != selfUpgradeStateSchema ||
		installed.Generation != state.Generation ||
		installed.IncumbentVersion != state.IncumbentVersion ||
		installed.IncumbentSHA256 != state.IncumbentSHA256 ||
		!reflect.DeepEqual(installed.RefusedCandidates, state.RefusedCandidates) ||
		!reflect.DeepEqual(installed.Attempts, state.Attempts) {
		return errors.New("self-upgrade state changed after publication")
	}
	return nil
}

func writeSelfUpgradeJSONFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := selfUpgradeStateCreateTemp(dir, ".selfupgrade-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = selfUpgradeStateRemove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := selfUpgradeStateWrite(tmp, string(data)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := selfUpgradeStateRename(tmpName, path); err != nil {
		return err
	}
	if err := selfUpgradeSyncDir(dir); err != nil {
		return fmt.Errorf("sync self-upgrade state directory: %w", err)
	}
	return nil
}

func (controller *selfUpgradeController) maintain(ctx context.Context) error {
	if controller == nil || !controller.enabled || !controller.eligible {
		return nil
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
	if err := controller.refreshSelfUpgradeState(); err != nil {
		controller.lastObservation = nil
		if errors.Is(err, errSelfUpgradeAttemptTimestampUncertain) {
			controller.eligible = false
			controller.reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
		}
		return fmt.Errorf("self-upgrade deferred: refresh refusal state: %w", err)
	}
	if !controller.eligible {
		return nil
	}
	if err := controller.ensureStatePublished(); err != nil {
		return fmt.Errorf("self-upgrade deferred: %w", err)
	}
	preflight, err := selfUpgradeCaptureImage(controller.locator, controller.incumbent.EmbeddedVersion)
	if err != nil {
		controller.lastObservation = nil
		return nil
	}
	if controller.lastObservation != nil &&
		controller.lastObservation.ledgerVersion == controller.ledgerVersion &&
		sameSelfUpgradeFileIdentity(controller.lastObservation.evidence, preflight) {
		return nil
	}
	if sameSelfUpgradeContent(controller.incumbent, preflight) {
		controller.lastObservation = controller.observe(preflight, selfUpgradeActionUnchanged)
		return nil
	}
	candidate, err := selfUpgradeCaptureCandidate(controller.locator)
	if err != nil {
		controller.lastObservation = nil
		return fmt.Errorf("self-upgrade deferred for candidate %s: build info: %w", controller.locator, err)
	}
	if !sameSelfUpgradeFileIdentity(preflight, candidate) {
		controller.lastObservation = nil
		return nil
	}
	controller.lastObservation = controller.observe(candidate, "")
	if sameSelfUpgradeContent(controller.incumbent, candidate) {
		controller.lastObservation.action = selfUpgradeActionUnchanged
		return nil
	}
	if !selfupgrade.VersionStrictlyNewer(controller.incumbent.EmbeddedVersion, candidate.EmbeddedVersion) {
		controller.lastObservation.action = selfUpgradeActionUnchanged
		return nil
	}
	if selfupgrade.RefusedCandidatesContain(controller.refused, candidate) {
		controller.lastObservation.action = selfUpgradeActionRefused
		return nil
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
	argv := append([]string(nil), os.Args...)
	env := append([]string(nil), os.Environ()...)
	release, err := controller.recordAttempt(candidate)
	if err != nil {
		var refusal selfUpgradeAttemptRefusalError
		if errors.As(err, &refusal) {
			controller.lastObservation = controller.observe(candidate, selfUpgradeActionRefused)
			return err
		}
		controller.lastObservation = nil
		if errors.Is(err, errSelfUpgradeCandidateRefused) {
			controller.lastObservation = controller.observe(candidate, selfUpgradeActionRefused)
			return nil
		}
		if errors.Is(err, errSelfUpgradeAttemptSettled) {
			controller.lastObservation = controller.observe(candidate, selfUpgradeActionUnchanged)
			return nil
		}
		if errors.Is(err, errSelfUpgradeAttemptTimestampUncertain) {
			controller.eligible = false
			controller.reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
			return fmt.Errorf("self-upgrade unavailable: %w", err)
		}
		return fmt.Errorf("self-upgrade deferred: persist replacement attempt: %w", err)
	}
	controller.lastObservation.ledgerVersion = controller.ledgerVersion
	execErr := selfUpgradeExecImage(candidate, argv, env)
	releaseErr := release()
	if releaseErr != nil {
		if execErr != nil {
			return errors.Join(
				fmt.Errorf("exec newer self-upgrade image: %w", execErr),
				fmt.Errorf("release self-upgrade state lock: %w", releaseErr),
			)
		}
		return fmt.Errorf("release self-upgrade state lock: %w", releaseErr)
	}
	if execErr != nil {
		return controller.refuse(candidate, fmt.Errorf("exec newer self-upgrade image: %w", execErr))
	}
	controller.lastObservation.action = selfUpgradeActionExec
	return nil
}

func (controller *selfUpgradeController) recordAttempt(candidate selfupgrade.ImageEvidence) (func() error, error) {
	release, err := selfUpgradeAcquireLock(controller.statePath)
	if err != nil {
		return nil, err
	}
	releaseWithError := func(failure error) (func() error, error) {
		releaseErr := release()
		if releaseErr != nil {
			return nil, errors.Join(failure, fmt.Errorf("release self-upgrade state lock: %w", releaseErr))
		}
		return nil, failure
	}
	current, exists, err := readSelfUpgradeStateFile(controller.statePath)
	if err != nil {
		return releaseWithError(err)
	}
	now := selfUpgradeNow()
	attempt := selfupgrade.NewAttempt(candidate, now)
	if err := selfupgrade.ValidateAttempt(attempt); err != nil {
		return releaseWithError(err)
	}
	state, err := controller.mergeStateAt(current, exists, now)
	if err != nil {
		return releaseWithError(err)
	}
	for _, existing := range state.Attempts {
		if existing.Status != selfupgrade.AttemptStatusSettled || !existing.Matches(candidate) {
			continue
		}
		controller.refused = append([]selfupgrade.RefusedCandidate(nil), state.RefusedCandidates...)
		controller.attempts = append([]selfupgrade.Attempt(nil), state.Attempts...)
		controller.ledgerVersion = selfUpgradeLedgerVersion(state.Generation, state.RefusedCandidates, state.Attempts)
		return release, nil
	}
	if selfupgrade.RefusedCandidatesContain(state.RefusedCandidates, candidate) {
		return releaseWithError(errSelfUpgradeCandidateRefused)
	}
	var refusal *selfUpgradeAttemptRefusalError
	for _, existing := range state.Attempts {
		if existing.Status != selfupgrade.AttemptStatusAttempt || !existing.Matches(candidate) {
			continue
		}
		if existing.IsFresh(now) {
			state.RefusedCandidates = selfupgrade.RememberRefusal(state.RefusedCandidates, candidate)
			candidateRefusal := selfUpgradeAttemptRefusalError{attempt: existing}
			refusal = &candidateRefusal
		}
		if refusal != nil {
			break
		}
	}
	if refusal == nil {
		attempts, err := selfupgrade.AddAttempt(state.Attempts, attempt, now)
		if err != nil {
			return releaseWithError(err)
		}
		if err := validateSelfUpgradeAttemptTimes(attempts, now); err != nil {
			return releaseWithError(err)
		}
		state.Attempts = attempts
	}
	state.Schema = selfUpgradeStateSchema
	state.Attempt = nil
	state.Attempts = nilIfEmptySelfUpgradeAttempts(state.Attempts)
	state.RefusedCandidates = nilIfEmptySelfUpgradeRefusals(state.RefusedCandidates)
	if err := validateSelfUpgradeRefusals(state.RefusedCandidates); err != nil {
		return releaseWithError(err)
	}
	if err := selfupgrade.ValidateAttempts(state.Attempts); err != nil {
		return releaseWithError(err)
	}
	if err := controller.writeState(state); err != nil {
		return releaseWithError(err)
	}
	controller.refused = append([]selfupgrade.RefusedCandidate(nil), state.RefusedCandidates...)
	controller.attempts = append([]selfupgrade.Attempt(nil), state.Attempts...)
	controller.statePublished = true
	controller.ledgerVersion = selfUpgradeLedgerVersion(state.Generation, state.RefusedCandidates, state.Attempts)
	if refusal != nil {
		return releaseWithError(*refusal)
	}
	return release, nil
}

func (controller *selfUpgradeController) markSettled() error {
	if controller == nil || !controller.enabled || !controller.eligible || controller.statePath == "" {
		return nil
	}
	if err := controller.publishState(func(state *selfUpgradeStateFile) error {
		settle := false
		for index := range state.Attempts {
			if state.Attempts[index].Status == selfupgrade.AttemptStatusAttempt &&
				state.Attempts[index].Matches(controller.incumbent) {
				state.Attempts[index].Status = selfupgrade.AttemptStatusSettled
				settle = true
			}
		}
		if !settle {
			return errSelfUpgradeNoMatchingAttempt
		}
		return nil
	}); err != nil {
		if errors.Is(err, errSelfUpgradeNoMatchingAttempt) {
			return nil
		}
		if errors.Is(err, errSelfUpgradeAttemptTimestampUncertain) {
			controller.eligible = false
			controller.reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
		}
		return fmt.Errorf("persist settled self-upgrade attempt: %w", err)
	}
	return nil
}

func (controller *selfUpgradeController) refuse(candidate selfupgrade.ImageEvidence, cause error) error {
	if err := controller.publishState(func(state *selfUpgradeStateFile) error {
		state.RefusedCandidates = selfupgrade.RememberRefusal(state.RefusedCandidates, candidate)
		return nil
	}); err != nil {
		controller.lastObservation = nil
		if errors.Is(err, errSelfUpgradeAttemptTimestampUncertain) {
			controller.eligible = false
			controller.reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
		}
		return errors.Join(cause, fmt.Errorf("persist self-upgrade refusal: %w", err))
	}
	controller.lastObservation = controller.observe(candidate, selfUpgradeActionRefused)
	return cause
}

func sameSelfUpgradeContent(first, second selfupgrade.ImageEvidence) bool {
	return first.Platform == second.Platform &&
		first.Size == second.Size &&
		first.SHA256 == second.SHA256
}

func sameSelfUpgradeFileIdentity(first, second selfupgrade.ImageEvidence) bool {
	return sameSelfUpgradeContent(first, second) &&
		first.Device == second.Device && first.Inode == second.Inode
}

func selfUpgradeEvidenceIdentityString(evidence selfupgrade.ImageEvidence) string {
	return fmt.Sprintf(
		"platform=%s,dev=%d,ino=%d,size=%d,sha256=%s,version=%s",
		evidence.Platform,
		evidence.Device,
		evidence.Inode,
		evidence.Size,
		evidence.SHA256,
		evidence.EmbeddedVersion,
	)
}
