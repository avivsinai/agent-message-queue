//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	envWakeNoSelfUpgrade          = "AMQ_WAKE_NO_SELF_UPGRADE"
	wakeSelfUpgradeFileName       = ".wake.selfupgrade"
	wakeSelfUpgradeSchemaV1       = 1
	wakeSelfUpgradeVersionTimeout = 5 * time.Second
	wakeSelfUpgradeRefusalLimit   = 8

	wakeSelfUpgradeActionDisabled         = "disabled"
	wakeSelfUpgradeActionIneligible       = "ineligible"
	wakeSelfUpgradeActionNoCandidate      = "no_candidate"
	wakeSelfUpgradeActionUnchanged        = "unchanged"
	wakeSelfUpgradeActionPrefilterRefused = "prefilter_refused"
	wakeSelfUpgradeActionRefused          = "refused"
	wakeSelfUpgradeActionRestartPending   = "restart_pending"
	wakeSelfUpgradeActionRefusedMemory    = "refused_memory"
	wakeSelfUpgradeActionRestartPresent   = "restart_present"
	wakeSelfUpgradeActionPending          = "pending"
	wakeSelfUpgradeActionDeferred         = "deferred"
	wakeSelfUpgradeActionRefusalPending   = "refusal_pending"
)

// wakeSelfUpgradeState is process-local trigger state. The locator is captured
// once, before the wake loop starts; it is intentionally not derived from the
// resolved image recorded in .wake.lock.
type wakeSelfUpgradeState struct {
	Enabled        bool
	Eligible       bool
	Locator        string
	Reason         string
	lastProbe      wakeSelfUpgradeProbe
	restartPending bool
	refusalPending *wakeSelfUpgradeRefusalPending
}

type wakeSelfUpgradeRefusalPending struct {
	Record wakeRestartRecord
	Reason string
}

type wakeSelfUpgradeProbe struct {
	Path     string
	Identity wakeFileIdentity
}

type wakeSelfUpgradeCandidate struct {
	Identity wakeFileIdentity `json:"identity"`
	Version  string           `json:"version"`
}

// wakeSelfUpgradeRefusedCandidate is the path-free identity used by the
// generation-scoped refusal memory in .wake.restart. Execution paths, methods,
// and ctimes are deliberately excluded because Darwin binding changes them.
type wakeSelfUpgradeRefusedCandidate struct {
	Platform        string `json:"platform"`
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	EmbeddedVersion string `json:"embedded_version"`
}

func wakeSelfUpgradeCandidateFromEvidence(evidence wakeImageEvidenceV1) *wakeSelfUpgradeCandidate {
	return &wakeSelfUpgradeCandidate{
		Identity: wakeFileIdentity{
			Device:    evidence.Device,
			Inode:     evidence.Inode,
			CTimeSec:  evidence.CTimeNS / int64(time.Second),
			CTimeNsec: evidence.CTimeNS % int64(time.Second),
		},
		Version: evidence.EmbeddedVersion,
	}
}

func wakeSelfUpgradeRefusedCandidateFromEvidence(evidence wakeImageEvidenceV1) wakeSelfUpgradeRefusedCandidate {
	return wakeSelfUpgradeRefusedCandidate{
		Platform:        evidence.Platform,
		Device:          evidence.Device,
		Inode:           evidence.Inode,
		Size:            evidence.Size,
		SHA256:          evidence.SHA256,
		EmbeddedVersion: evidence.EmbeddedVersion,
	}
}

func sameWakeSelfUpgradeRefusedCandidates(
	first, second []wakeSelfUpgradeRefusedCandidate,
) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func wakeSelfUpgradeRefusedCandidatesContain(
	candidates []wakeSelfUpgradeRefusedCandidate,
	evidence wakeImageEvidenceV1,
) bool {
	want := wakeSelfUpgradeRefusedCandidateFromEvidence(evidence)
	for _, candidate := range candidates {
		if candidate == want {
			return true
		}
	}
	return false
}

// rememberWakeSelfUpgradeRefusal appends the current identity as the most
// recent distinct refusal and retains at most the eight most recent entries.
func rememberWakeSelfUpgradeRefusal(
	candidates []wakeSelfUpgradeRefusedCandidate,
	evidence wakeImageEvidenceV1,
) []wakeSelfUpgradeRefusedCandidate {
	current := wakeSelfUpgradeRefusedCandidateFromEvidence(evidence)
	remembered := make([]wakeSelfUpgradeRefusedCandidate, 0, len(candidates)+1)
	for _, candidate := range candidates {
		if candidate != current {
			remembered = append(remembered, candidate)
		}
	}
	remembered = append(remembered, current)
	if len(remembered) > wakeSelfUpgradeRefusalLimit {
		remembered = append(
			[]wakeSelfUpgradeRefusedCandidate(nil),
			remembered[len(remembered)-wakeSelfUpgradeRefusalLimit:]...,
		)
	}
	return remembered
}

func wakeSelfUpgradeRefusalMemory(record wakeRestartRecord) []wakeSelfUpgradeRefusedCandidate {
	remembered := append([]wakeSelfUpgradeRefusedCandidate(nil), record.RefusedCandidates...)
	if record.Status == wakeRestartRefused && len(remembered) == 0 {
		remembered = rememberWakeSelfUpgradeRefusal(remembered, record.Candidate)
	}
	return remembered
}

func validateWakeSelfUpgradeRefusedCandidates(record wakeRestartRecord) error {
	if len(record.RefusedCandidates) == 0 {
		return nil
	}
	if record.Source != wakeRestartSourceSelf {
		return fmt.Errorf("wake restart refused candidates require a self-upgrade source")
	}
	if len(record.RefusedCandidates) > wakeSelfUpgradeRefusalLimit {
		return fmt.Errorf(
			"wake restart refused candidates exceed the limit of %d",
			wakeSelfUpgradeRefusalLimit,
		)
	}
	seen := make(map[wakeSelfUpgradeRefusedCandidate]struct{}, len(record.RefusedCandidates))
	for _, candidate := range record.RefusedCandidates {
		if candidate.Platform != record.Candidate.Platform || candidate.Device == 0 ||
			candidate.Inode == 0 || candidate.Size <= 0 ||
			!validWakeImageSHA256(candidate.SHA256) ||
			strings.TrimSpace(candidate.EmbeddedVersion) == "" ||
			candidate.EmbeddedVersion != strings.TrimSpace(candidate.EmbeddedVersion) ||
			strings.ContainsRune(candidate.EmbeddedVersion, 0) {
			return fmt.Errorf("wake restart refused candidate identity is invalid")
		}
		if _, exists := seen[candidate]; exists {
			return fmt.Errorf("wake restart refused candidates contain a duplicate identity")
		}
		seen[candidate] = struct{}{}
	}
	containsCurrent := wakeSelfUpgradeRefusedCandidatesContain(
		record.RefusedCandidates,
		record.Candidate,
	)
	if record.Status == wakeRestartPending && containsCurrent {
		return fmt.Errorf("pending wake restart candidate is already in refusal memory")
	}
	if record.Status == wakeRestartRefused && !containsCurrent {
		return fmt.Errorf("refused wake restart candidate is missing from refusal memory")
	}
	return nil
}

type wakeSelfUpgradeDecision struct {
	Action    string
	Reason    string
	Candidate *wakeSelfUpgradeCandidate
}

type wakeSelfUpgradeDiagnostic struct {
	Schema        int                               `json:"schema"`
	Root          string                            `json:"root"`
	Agent         string                            `json:"agent"`
	Generation    string                            `json:"generation"`
	Enabled       bool                              `json:"enabled"`
	Eligible      bool                              `json:"eligible"`
	Locator       string                            `json:"locator,omitempty"`
	LastCandidate *wakeSelfUpgradeCandidate         `json:"last_candidate,omitempty"`
	LastDecision  wakeSelfUpgradeDiagnosticDecision `json:"last_decision"`
}

type wakeSelfUpgradeDiagnosticDecision struct {
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

var (
	wakeSelfUpgradeLookPath       = exec.LookPath
	wakeSelfUpgradeEvalSymlinks   = filepath.EvalSymlinks
	wakeSelfUpgradeLstat          = os.Lstat
	wakeSelfUpgradeStat           = os.Stat
	wakeSelfUpgradeRunVersion     = runWakeSelfUpgradeVersion
	wakeSelfUpgradeNow            = time.Now
	wakeSelfUpgradeLiveDifference = inspectWakeSelfUpgradeLiveDifference
	wakeSelfUpgradeReadPublished  = readWakeRestartRecordAt
	wakeSelfUpgradeInspectLockAt  = inspectWakeLockAt
)

func coopWakeLaunchLocator(argv0 string) string {
	if locator, err := exec.LookPath(strings.TrimSpace(argv0)); err == nil {
		if filepath.IsAbs(locator) && filepath.Clean(locator) == locator {
			return locator
		}
	}
	return ""
}

func coopWakeExecutionPath(executable string) (string, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", fmt.Errorf("current amq executable path is empty")
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve absolute current amq executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve current amq executable symlinks: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("resolved current amq executable path is not absolute")
	}
	return resolved, nil
}

func wakeSelfUpgradeDisabledByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envWakeNoSelfUpgrade))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// captureWakeSelfUpgradeStartupState never follows the launch locator for
// storage. It only resolves once to establish the baseline identity that each
// maintenance tick will compare against.
func captureWakeSelfUpgradeStartupState(
	argv0 string,
	enabled bool,
	running wakeImageEvidenceV1,
) wakeSelfUpgradeState {
	state := wakeSelfUpgradeState{Enabled: enabled}
	if !enabled {
		state.Reason = "disabled by --no-self-upgrade or " + envWakeNoSelfUpgrade
		return state
	}
	if err := validateWakeImageEvidence(running); err != nil {
		state.Reason = "running image evidence is unavailable"
		return state
	}
	locator, err := wakeSelfUpgradeLookPath(strings.TrimSpace(argv0))
	if err != nil {
		state.Reason = "launch locator is unavailable"
		return state
	}
	locator = strings.TrimSpace(locator)
	if locator == "" || !filepath.IsAbs(locator) || filepath.Clean(locator) != locator {
		state.Reason = "launch locator is not an absolute canonical path"
		return state
	}
	locatorInfo, err := wakeSelfUpgradeLstat(locator)
	if err != nil {
		state.Reason = "launch locator is unavailable"
		return state
	}
	if err := validateWakeSelfUpgradeLaunchLocator(locator, locatorInfo); err != nil {
		state.Reason = "launch locator is not safely owned"
		return state
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		state.Reason = "launch locator is not safely resolvable"
		return state
	}
	// A direct locator is already resolved and cannot observe an installer
	// symlink flip. Compare lstat identity rather than strings: Darwin commonly
	// reports equivalent /var and /private/var spellings. A symlink resolving to
	// the running image is the normal, eligible steady state.
	locatorIdentity, locatorIdentityOK := captureWakeFileIdentity(locatorInfo)
	if locatorIdentityOK && locatorIdentity == probe.Identity {
		state.Reason = "wake was launched from a pinned resolved image; restart it from the stable launch command"
		return state
	}
	state.Eligible = true
	state.Locator = locator
	startupInspection := wakeLockInspection{
		PID:               os.Getpid(),
		IdentityConfirmed: true,
		Process:           wakeProcessInfo{PID: os.Getpid(), Running: true},
		Lock: wakeLock{
			Started:              time.Now().UTC().Format(time.RFC3339Nano),
			ImagePath:            running.ExecutionPath,
			ImageVersion:         running.EmbeddedVersion,
			RunningImageEvidence: &running,
		},
	}
	if different, comparisonErr := wakeSelfUpgradeLiveDifference(startupInspection, probe); comparisonErr == nil && !different {
		state.lastProbe = probe
	}
	return state
}

func wakeSelfUpgradeProbeMatchesEvidence(
	probe wakeSelfUpgradeProbe,
	evidence wakeImageEvidenceV1,
) bool {
	return probe.Identity.Device == evidence.Device &&
		probe.Identity.Inode == evidence.Inode &&
		probe.Identity.CTimeSec == evidence.CTimeNS/int64(time.Second) &&
		probe.Identity.CTimeNsec == evidence.CTimeNS%int64(time.Second)
}

func validateWakeSelfUpgradeLaunchLocator(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink == 0 {
		return validateWakeTargetPathOwnership("wake self-upgrade launch locator", path, info)
	}
	// Symlink permission bits are not an authority signal. Linux reports them
	// as 0777 even though chmod does not control link traversal. Ownership of
	// the unresolved locator is still required; the resolved executable keeps
	// the existing strict mode and ownership validation in the per-tick probe.
	ownerUID, ownerOK := wakeTargetFileOwnerUID(info)
	currentUID, currentOK := wakeTargetCurrentUID()
	if ownerOK && currentOK && ownerUID != currentUID {
		return fmt.Errorf(
			"wake self-upgrade launch locator %s is owned by uid %d, want current uid %d",
			path,
			ownerUID,
			currentUID,
		)
	}
	return nil
}

func constrainWakeSelfUpgradeEligibility(
	state wakeSelfUpgradeState,
	resumeEligible bool,
) wakeSelfUpgradeState {
	if state.Enabled && !resumeEligible {
		state.Eligible = false
		state.Reason = "wake is not structurally resume-eligible for automatic self-upgrade"
	}
	return state
}

func probeWakeSelfUpgradeLocator(locator string) (wakeSelfUpgradeProbe, error) {
	locatorInfo, err := wakeSelfUpgradeLstat(locator)
	if err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	if err := validateWakeSelfUpgradeLaunchLocator(locator, locatorInfo); err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	resolved, err := wakeSelfUpgradeEvalSymlinks(locator)
	if err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	confirmedLocatorInfo, err := wakeSelfUpgradeLstat(locator)
	if err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	if !sameWakeFileIdentity(locatorInfo, confirmedLocatorInfo) {
		return wakeSelfUpgradeProbe{}, fmt.Errorf("launch locator changed while resolving")
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return wakeSelfUpgradeProbe{}, fmt.Errorf("resolved launch locator is not an absolute canonical path")
	}
	info, err := wakeSelfUpgradeStat(resolved)
	if err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return wakeSelfUpgradeProbe{}, fmt.Errorf("resolved launch locator is not an executable regular file")
	}
	if err := validateWakeTargetPathOwnership("wake self-upgrade locator", resolved, info); err != nil {
		return wakeSelfUpgradeProbe{}, err
	}
	identity, ok := captureWakeFileIdentity(info)
	if !ok || identity.Device == 0 || identity.Inode == 0 {
		return wakeSelfUpgradeProbe{}, fmt.Errorf("resolved launch locator identity is unavailable")
	}
	return wakeSelfUpgradeProbe{Path: resolved, Identity: identity}, nil
}

func sameWakeSelfUpgradeProbe(first, second wakeSelfUpgradeProbe) bool {
	return first.Path == second.Path && first.Identity == second.Identity
}

func inspectWakeSelfUpgradeLiveDifference(
	inspection wakeLockInspection,
	probe wakeSelfUpgradeProbe,
) (bool, error) {
	info, err := os.Stat(probe.Path)
	if err != nil {
		return false, fmt.Errorf("stat candidate image for live comparison: %w", err)
	}
	identity, ok := captureWakeFileIdentity(info)
	if !ok || identity != probe.Identity {
		return false, fmt.Errorf("candidate image changed before live comparison")
	}
	comparison, err := inspectWakeBinaryStalenessPlatform(
		inspection,
		resolvedWakeBinary{Path: probe.Path, Info: info},
	)
	if err != nil {
		return false, err
	}
	switch comparison.Method {
	case wakeBinaryComparisonExactIdentity,
		wakeBinaryComparisonDarwinProcessImage,
		wakeBinaryComparisonDarwinDeletedImage:
		return comparison.Stale, nil
	default:
		return false, fmt.Errorf("live wake image comparison is not conclusive")
	}
}

func runWakeSelfUpgradeVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wakeSelfUpgradeVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("candidate version probe timed out: %w", ctx.Err())
		}
		return "", fmt.Errorf("candidate version probe: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" || strings.ContainsAny(version, "\r\n\t ") {
		return "", fmt.Errorf("candidate version probe returned a malformed version")
	}
	return version, nil
}

// maintainWakeSelfUpgrade performs one maintenance-tick decision. It never
// binds, notifies, preflights, or execs: those remain record-owned restart
// control-plane responsibilities.
func maintainWakeSelfUpgrade(
	state *wakeSelfUpgradeState,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (wakeSelfUpgradeDecision, error) {
	if state == nil {
		return wakeSelfUpgradeDecision{Action: wakeSelfUpgradeActionDisabled, Reason: "self-upgrade state is unavailable"}, nil
	}
	decision := wakeSelfUpgradeDecision{Action: wakeSelfUpgradeActionDisabled, Reason: state.Reason}
	if !state.Enabled {
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	if !state.Eligible {
		decision.Action = wakeSelfUpgradeActionIneligible
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	if agentDir == nil {
		return wakeSelfUpgradeDecision{}, fmt.Errorf("wake self-upgrade agent directory capability is missing")
	}
	if !inspection.Exists || inspection.Status != wakeLockValid || !inspection.IdentityConfirmed ||
		!inspection.Process.Running {
		decision.Action = wakeSelfUpgradeActionIneligible
		decision.Reason = "wake is not a live identity-confirmed process"
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	if err := validateWakeResumeAdvertisement(inspection.Lock, inspection.Root, inspection.Agent); err != nil {
		decision.Action = wakeSelfUpgradeActionIneligible
		decision.Reason = err.Error()
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}

	probe, err := probeWakeSelfUpgradeLocator(state.Locator)
	if err != nil {
		decision.Action = wakeSelfUpgradeActionNoCandidate
		decision.Reason = "stable launch locator is unavailable; retrying next maintenance tick"
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	if sameWakeSelfUpgradeProbe(state.lastProbe, probe) {
		return wakeSelfUpgradeDecision{Action: wakeSelfUpgradeActionUnchanged}, nil
	}
	version, err := wakeSelfUpgradeRunVersion(probe.Path)
	if err != nil {
		decision.Action = wakeSelfUpgradeActionNoCandidate
		decision.Reason = err.Error()
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	candidate := &wakeSelfUpgradeCandidate{Identity: probe.Identity, Version: version}
	decision.Candidate = candidate
	if !wakeSelfUpgradeVersionStrictlyNewer(inspection.Lock.ImageVersion, version) {
		state.lastProbe = probe
		decision.Action = wakeSelfUpgradeActionPrefilterRefused
		decision.Reason = wakeRestartReasonWithRemedy(
			"candidate version is not strictly newer than the running wake",
			inspection.Root,
			inspection.Agent,
		)
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}

	evidence, err := captureWakeImageEvidence(probe.Path, version)
	if err != nil {
		decision.Action = wakeSelfUpgradeActionNoCandidate
		decision.Reason = wakeRestartReasonWithRemedy(
			"capture candidate image evidence: "+err.Error(),
			inspection.Root,
			inspection.Agent,
		)
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	decision.Candidate = wakeSelfUpgradeCandidateFromEvidence(evidence)
	if !wakeSelfUpgradeProbeMatchesEvidence(probe, evidence) {
		state.lastProbe = probe
		decision.Action = wakeSelfUpgradeActionRefused
		decision.Reason = wakeRestartReasonWithRemedy(
			"candidate image changed while capturing evidence",
			inspection.Root,
			inspection.Agent,
		)
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	different, comparisonErr := wakeSelfUpgradeLiveDifference(inspection, probe)
	if comparisonErr != nil {
		// Inconclusive identity is transient (Homebrew Cellar unlink, proc_pidpath
		// ESRCH). Do not remember the probe or write refusal memory; retry next tick.
		decision.Action = wakeSelfUpgradeActionDeferred
		decision.Reason = wakeRestartReasonWithRemedy(
			"live wake image identity is unknown or ambiguous: "+comparisonErr.Error(),
			inspection.Root,
			inspection.Agent,
		)
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}
	if !different {
		state.lastProbe = probe
		decision.Action = wakeSelfUpgradeActionRefused
		decision.Reason = wakeRestartReasonWithRemedy(
			"candidate image identity is not conclusively different from the live wake",
			inspection.Root,
			inspection.Agent,
		)
		return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
	}

	requestID, err := newWakeRestartRequestID()
	if err != nil {
		return wakeSelfUpgradeDecision{}, err
	}
	record := wakeRestartRecord{
		Schema:             wakeRestartSchemaV1,
		RequestID:          requestID,
		Status:             wakeRestartPending,
		Source:             wakeRestartSourceSelf,
		Root:               inspection.Root,
		Agent:              inspection.Agent,
		Generation:         inspection.Lock.Generation,
		Owner:              *inspection.Lock.ResumeOwner,
		Candidate:          evidence,
		PreviousBoundImage: previousDarwinWakeRestartStageForLock(inspection.Lock),
	}
	stagePath, err := planWakeRestartStageForRecordPlatform(record)
	if err != nil {
		return wakeSelfUpgradeDecision{}, err
	}
	record.StagePath = stagePath
	decision, err = publishWakeSelfUpgradePending(agentDir, inspection, record, decision)
	if err != nil {
		return decision, err
	}
	if decision.Action != wakeSelfUpgradeActionRestartPending &&
		decision.Action != wakeSelfUpgradeActionRestartPresent {
		state.lastProbe = probe
	}
	return decision, recordWakeSelfUpgradeDecision(agentDir, inspection, *state, decision)
}

func publishWakeSelfUpgradePending(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	record wakeRestartRecord,
	decision wakeSelfUpgradeDecision,
) (wakeSelfUpgradeDecision, error) {
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade publication")
		}
		existing, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if exists {
			switch {
			case existing.Record.Schema == wakeRestartSchemaV1 &&
				existing.Record.Status == wakeRestartPending &&
				existing.Record.Source == wakeRestartSourceSelf &&
				existing.Record.Root == expected.Root &&
				existing.Record.Agent == expected.Agent &&
				existing.Record.Generation == expected.Lock.Generation &&
				expected.Lock.ResumeOwner != nil &&
				sameWakeOwner(&existing.Record.Owner, expected.Lock.ResumeOwner) &&
				sameOptionalWakeImageEvidence(
					existing.Record.PreviousBoundImage,
					previousDarwinWakeRestartStageForLock(expected.Lock),
				) &&
				sameWakeSelfUpgradeCandidateIdentity(existing.Record.Candidate, record.Candidate):
				decision.Action = wakeSelfUpgradeActionPending
				decision.Reason = "existing self-upgrade restart request was adopted"
				decision.Candidate = wakeSelfUpgradeCandidateFromEvidence(existing.Record.Candidate)
				return nil
			case existing.Record.Status == wakeRestartPending:
				decision.Action = wakeSelfUpgradeActionRestartPending
				decision.Reason = "an existing wake restart request is preserved"
				return nil
			case existing.Record.Status == wakeRestartRefused &&
				existing.Record.Source == wakeRestartSourceSelf:
				sameScope := sameWakeSelfUpgradeRefusalScope(existing.Record, expected)
				if sameScope {
					remembered := wakeSelfUpgradeRefusalMemory(existing.Record)
					if wakeSelfUpgradeRefusedCandidatesContain(remembered, record.Candidate) {
						decision.Action = wakeSelfUpgradeActionRefusedMemory
						decision.Reason = "candidate was already refused for this wake generation"
						return nil
					}
					record.RefusedCandidates = remembered
				}
				if err := reclaimWakeRestartStagePlatform(existing.Record); err != nil {
					return fmt.Errorf("reclaim superseded self-upgrade attempt: %w", err)
				}
				if !sameScope {
					if _, err := quarantineWakeRestartRecordAt(dirfd, agentDir, existing); err != nil {
						return fmt.Errorf("quarantine superseded self-upgrade attempt: %w", err)
					}
				}
			default:
				decision.Action = wakeSelfUpgradeActionRestartPresent
				decision.Reason = "a foreign wake restart record is preserved"
				return nil
			}
		}
		if err := writeWakeRestartRecordAt(dirfd, agentDir, record); err != nil {
			return err
		}
		installed, installedExists, err := wakeSelfUpgradeReadPublished(dirfd, agentDir)
		if err != nil || !installedExists || !sameWakeRestartRecord(record, installed) {
			return fmt.Errorf("self-upgrade restart request changed after publication")
		}
		decision.Action = wakeSelfUpgradeActionPending
		decision.Reason = "self-upgrade restart request is pending"
		return nil
	})
	return decision, err
}

func sameWakeSelfUpgradeRefusalScope(
	record wakeRestartRecord,
	expected wakeLockInspection,
) bool {
	return record.Schema == wakeRestartSchemaV1 &&
		record.SuccessorGeneration == "" &&
		record.Status == wakeRestartRefused &&
		record.Source == wakeRestartSourceSelf &&
		record.Root == expected.Root &&
		record.Agent == expected.Agent &&
		record.Generation == expected.Lock.Generation &&
		expected.Lock.ResumeOwner != nil &&
		sameWakeOwner(&record.Owner, expected.Lock.ResumeOwner) &&
		sameOptionalWakeImageEvidence(
			record.PreviousBoundImage,
			previousDarwinWakeRestartStageForLock(expected.Lock),
		)
}

// Darwin staging necessarily changes the candidate inode ctime and execution
// path. Failed-attempt memory therefore keys the stable image identity, not
// those binding-method artifacts.
func sameWakeSelfUpgradeCandidateIdentity(first, second wakeImageEvidenceV1) bool {
	return first.Platform == second.Platform && first.Device == second.Device &&
		first.Inode == second.Inode && first.Size == second.Size &&
		first.SHA256 == second.SHA256 && first.EmbeddedVersion == second.EmbeddedVersion
}

func wakeSelfUpgradeEvidenceIdentityString(evidence wakeImageEvidenceV1) string {
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

func readWakeSelfUpgradeDiagnosticAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (wakeSelfUpgradeDiagnostic, bool) {
	if agentDir == nil {
		return wakeSelfUpgradeDiagnostic{}, false
	}
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeSelfUpgradeFileName,
		"wake self-upgrade diagnostic",
		filepath.Join(agentDir.path, wakeSelfUpgradeFileName),
		maxWakeMetadataFileBytes,
	)
	if err != nil || !exists {
		return wakeSelfUpgradeDiagnostic{}, false
	}
	var diagnostic wakeSelfUpgradeDiagnostic
	if json.Unmarshal(raw, &diagnostic) != nil ||
		diagnostic.Schema != wakeSelfUpgradeSchemaV1 ||
		diagnostic.Root != inspection.Root || diagnostic.Agent != inspection.Agent ||
		diagnostic.Generation != inspection.Lock.Generation {
		return wakeSelfUpgradeDiagnostic{}, false
	}
	return diagnostic, true
}

func recordWakeSelfUpgradeDecision(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	state wakeSelfUpgradeState,
	decision wakeSelfUpgradeDecision,
) error {
	if agentDir == nil || !inspection.Exists || inspection.Lock.Generation == "" {
		return nil
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
		if !current.Exists || inspection.Lock.Generation == "" ||
			current.Lock.Generation != inspection.Lock.Generation ||
			current.Root != inspection.Root || current.Agent != inspection.Agent {
			return fmt.Errorf("wake changed before self-upgrade diagnostic publication")
		}
		previous, exists := readWakeSelfUpgradeDiagnosticAt(dirfd, agentDir, inspection)
		lastCandidate := decision.Candidate
		if lastCandidate == nil && exists {
			lastCandidate = previous.LastCandidate
		}
		diagnostic := wakeSelfUpgradeDiagnostic{
			Schema:        wakeSelfUpgradeSchemaV1,
			Root:          inspection.Root,
			Agent:         inspection.Agent,
			Generation:    inspection.Lock.Generation,
			Enabled:       state.Enabled,
			Eligible:      state.Eligible,
			Locator:       state.Locator,
			LastCandidate: lastCandidate,
			LastDecision: wakeSelfUpgradeDiagnosticDecision{
				Action: decision.Action,
				Reason: decision.Reason,
				At:     wakeSelfUpgradeNow().UTC(),
			},
		}
		if exists && sameWakeSelfUpgradeDiagnostic(previous, diagnostic) {
			return nil
		}
		raw, err := json.Marshal(diagnostic)
		if err != nil {
			return err
		}
		return writeWakeRepairMetadataAt(
			dirfd,
			agentDir,
			wakeSelfUpgradeFileName,
			"wake self-upgrade diagnostic",
			append(raw, '\n'),
			maxWakeMetadataFileBytes,
		)
	})
}

func sameWakeSelfUpgradeDiagnostic(first, second wakeSelfUpgradeDiagnostic) bool {
	if first.Schema != second.Schema || first.Root != second.Root || first.Agent != second.Agent ||
		first.Generation != second.Generation || first.Enabled != second.Enabled ||
		first.Eligible != second.Eligible || first.Locator != second.Locator ||
		first.LastDecision.Action != second.LastDecision.Action ||
		first.LastDecision.Reason != second.LastDecision.Reason {
		return false
	}
	if first.LastCandidate == nil || second.LastCandidate == nil {
		return first.LastCandidate == nil && second.LastCandidate == nil
	}
	return *first.LastCandidate == *second.LastCandidate
}

func removeWakeSelfUpgradeDiagnosticAt(dirfd int) error {
	err := unix.Unlinkat(dirfd, wakeSelfUpgradeFileName, 0)
	if err == unix.ENOENT {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove wake self-upgrade diagnostic: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake self-upgrade diagnostic removal: %w", err)
	}
	return nil
}

func removeWakeSelfUpgradeDiagnosticGuarded(root, agent string) error {
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return agentDir.withFD(func(dirfd int) error {
		return removeWakeSelfUpgradeDiagnosticAt(dirfd)
	})
}
