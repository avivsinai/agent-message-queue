package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

const (
	selfUpgradeStateSchema     = 1
	selfUpgradeStateFileName   = ".selfupgrade.json"
	selfUpgradeActionUnchanged = "unchanged"
	selfUpgradeActionRefused   = "refused"
	selfUpgradeActionExec      = "exec"
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
)

type selfUpgradeController struct {
	enabled         bool
	eligible        bool
	reason          string
	locator         string
	incumbent       selfupgrade.ImageEvidence
	generation      string
	statePath       string
	refused         []selfupgrade.RefusedCandidate
	statePublished  bool
	lastObservation *selfUpgradeObservation
}

type selfUpgradeObservation struct {
	evidence selfupgrade.ImageEvidence
	action   string
}

type selfUpgradeStateFile struct {
	Schema            int                            `json:"schema"`
	Generation        string                         `json:"generation"`
	IncumbentVersion  string                         `json:"incumbent_version"`
	IncumbentSHA256   string                         `json:"incumbent_sha256"`
	RefusedCandidates []selfupgrade.RefusedCandidate `json:"refused_candidates,omitempty"`
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
	info, err := selfUpgradeStateFileInfo(controller.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("self-upgrade refusal state is not a private regular file")
	}
	raw, err := selfUpgradeStateReadFile(controller.statePath)
	if err != nil {
		return err
	}
	var state selfUpgradeStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Schema != selfUpgradeStateSchema || strings.TrimSpace(state.Generation) == "" {
		return errors.New("self-upgrade refusal state schema is invalid")
	}
	if err := validateSelfUpgradeRefusals(state.RefusedCandidates); err != nil {
		return err
	}
	if state.Generation == controller.generation {
		controller.refused = append([]selfupgrade.RefusedCandidate(nil), state.RefusedCandidates...)
	}
	return nil
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
	if controller.statePath == "" {
		return errors.New("self-upgrade state path is empty")
	}
	state := selfUpgradeStateFile{
		Schema:            selfUpgradeStateSchema,
		Generation:        controller.generation,
		IncumbentVersion:  controller.incumbent.EmbeddedVersion,
		IncumbentSHA256:   controller.incumbent.SHA256,
		RefusedCandidates: append([]selfupgrade.RefusedCandidate(nil), controller.refused...),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(controller.statePath)
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
	if err := selfUpgradeStateRename(tmpName, controller.statePath); err != nil {
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
	if err := controller.ensureStatePublished(); err != nil {
		return fmt.Errorf("self-upgrade deferred: %w", err)
	}
	preflight, err := selfUpgradeCaptureImage(controller.locator, controller.incumbent.EmbeddedVersion)
	if err != nil {
		controller.lastObservation = nil
		return nil
	}
	if controller.lastObservation != nil &&
		sameSelfUpgradeFileIdentity(controller.lastObservation.evidence, preflight) {
		return nil
	}
	if sameSelfUpgradeContent(controller.incumbent, preflight) {
		controller.lastObservation = &selfUpgradeObservation{evidence: preflight, action: selfUpgradeActionUnchanged}
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
	controller.lastObservation = &selfUpgradeObservation{evidence: candidate}
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
	if err := selfUpgradeExecImage(candidate, argv, env); err != nil {
		return controller.refuse(candidate, fmt.Errorf("exec newer self-upgrade image: %w", err))
	}
	controller.lastObservation.action = selfUpgradeActionExec
	return nil
}

func (controller *selfUpgradeController) refuse(candidate selfupgrade.ImageEvidence, cause error) error {
	controller.refused = selfupgrade.RememberRefusal(controller.refused, candidate)
	controller.lastObservation = &selfUpgradeObservation{evidence: candidate, action: selfUpgradeActionRefused}
	if err := controller.saveState(); err != nil {
		return errors.Join(cause, fmt.Errorf("persist self-upgrade refusal: %w", err))
	}
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
