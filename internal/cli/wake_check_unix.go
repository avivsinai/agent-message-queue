//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeCheckResult struct {
	Schema                   int    `json:"schema"`
	Agent                    string `json:"agent"`
	Root                     string `json:"root"`
	CanStartHere             bool   `json:"can_start_here"`
	StartMode                string `json:"start_mode"`
	StartReason              string `json:"start_reason,omitempty"`
	LiveWake                 bool   `json:"live_wake"`
	WakeStatus               string `json:"wake_status"`
	WakePID                  int    `json:"wake_pid,omitempty"`
	WakeMode                 string `json:"wake_mode,omitempty"`
	OwnerBound               bool   `json:"owner_bound"`
	RunningImagePath         string `json:"running_image_path"`
	RunningVersion           string `json:"running_version"`
	CurrentImagePath         string `json:"current_image_path"`
	CurrentVersion           string `json:"current_version"`
	ImageStatus              string `json:"image_status"`
	CanRepairInjectVia       bool   `json:"can_repair_inject_via"`
	RepairReason             string `json:"repair_reason,omitempty"`
	RestartCapability        string `json:"restart_capability"`
	OperatorTerminalRequired bool   `json:"operator_terminal_required"`
	NextAction               string `json:"next_action"`
}

type wakeStartCapability struct {
	CanStart bool
	Mode     string
	Reason   string
}

func wakeCheckStartReason(start wakeStartCapability) (*string, *string) {
	if start.CanStart {
		return nil, nil
	}
	reason := wakeReasonOwningTerminalRequired
	if start.Mode == wakeInjectModeNone {
		reason = wakeReasonFullStrengthUnavailable
	}
	return &reason, wakeCheckOptionalString(start.Reason)
}

type wakeCheckMetadataFingerprint struct {
	Exists   bool
	Identity wakeFileIdentity
	Digest   string
}

type wakeCheckObservation struct {
	Inspection  wakeLockInspection
	Target      wakeCheckMetadataFingerprint
	Floor       wakeCheckMetadataFingerprint
	Repair      wakeRepairAssessment
	SelfUpgrade wakeCheckSelfUpgradeObservation
}

type wakeCheckSnapshot struct {
	OpsLock  *opsWakeLock
	Decision wakeCheckDecision
}

func runWakeCheck(args []string) error {
	fs := flag.NewFlagSet("wake check", flag.ContinueOnError)
	common := addCommonFlags(fs)
	jsonSchema := addJSONSchemaFlag(fs)
	usage := usageWithFlags(
		fs,
		"amq wake check --me <agent> [options]",
		"Inspect wake start and restart capability without mutation.",
		"",
		"Reports whether this process can start a full-strength terminal wake,",
		"whether an existing wake is live or repairable, the running and current",
		"AMQ images, and the exact non-destructive next action.",
		"",
		"Only restart_capability=agent_safe authorizes an agent-side action.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := validateJSONSchemaFlag(fs, common.JSON, *jsonSchema); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	if err := validateWakeLockAgent(root, me); err != nil {
		return fmt.Errorf("inspect wake for %s: %w", me, err)
	}

	decision := inspectWakeCheckDecision(root, me)
	if common.JSON {
		if *jsonSchema == wakeCheckSchemaV2 {
			return writeJSON(os.Stdout, renderWakeCheckV2(decision))
		}
		return writeJSON(os.Stdout, renderWakeCheckV1(decision))
	}
	return writeWakeCheckText(renderWakeCheckV1(decision))
}

func inspectWakeCheck(root, me string) wakeCheckResult {
	return renderWakeCheckV1(inspectWakeCheckDecision(root, me))
}

func inspectWakeCheckDecision(root, me string) wakeCheckDecision {
	return inspectWakeCheckSnapshot(root, me).Decision
}

func inspectWakeCheckSnapshot(root, me string) wakeCheckSnapshot {
	root = canonicalWakeRoot(root)
	first, firstErr := observeWakeCheck(root, me)
	if firstErr != nil {
		return wakeCheckObservationFailureSnapshot(root, me, first, firstErr)
	}
	second, secondErr := observeWakeCheck(root, me)
	if secondErr != nil || !sameWakeCheckObservation(first, second) {
		return wakeCheckObservationFailureSnapshot(root, me, second, secondErr)
	}
	opsLock := opsWakeLockFromWakeCheckObservation(root, me, second)
	snapshot := wakeCheckSnapshot{
		OpsLock:  opsLock,
		Decision: buildWakeCheckDecision(root, me, second.Inspection, opsLock, true),
	}
	snapshot.Decision.SelfUpgrade = wakeCheckSelfUpgradeDecisionFromObservation(second.SelfUpgrade)
	// Image probing occurs while building the decision. Re-observe afterwards
	// so a PID reuse or lock generation change cannot inherit that conclusion.
	third, thirdErr := observeWakeCheck(root, me)
	if thirdErr != nil || !sameWakeCheckObservation(second, third) {
		return wakeCheckObservationFailureSnapshot(root, me, third, thirdErr)
	}
	return snapshot
}

func observeWakeCheck(root, me string) (wakeCheckObservation, error) {
	var observation wakeCheckObservation
	agentPath := fsq.AgentBase(root, me)
	if _, err := os.Lstat(agentPath); err != nil {
		if os.IsNotExist(err) {
			observation.Inspection = inspectWakeLock(root, me)
			return observation, nil
		}
		return observation, fmt.Errorf("stat wake agent directory: %w", err)
	}

	agentDir, err := openWakeDirectory(agentPath, "wake agent directory")
	if err != nil {
		return observation, err
	}
	defer func() { _ = agentDir.Close() }()

	err = agentDir.withFD(func(dirfd int) error {
		observation.Inspection = inspectWakeLockAt(dirfd, agentDir, root, me)
		observation.SelfUpgrade = observeWakeCheckSelfUpgradeAt(
			dirfd,
			agentDir,
			observation.Inspection,
		)
		var boundSelectionErr error
		observation.Repair = assessWakeRepair(
			root,
			me,
			observation.Inspection,
			func() (wakeTarget, bool, error) {
				selection, err := readWakeStateSelectionForInspectionAt(
					dirfd, agentDir, root, me, observation.Inspection,
				)
				fingerprintErr := recordWakeCheckSelectionTarget(&observation, selection)
				if fingerprintErr != nil {
					return selection.Target, selection.TargetPresent, fingerprintErr
				}
				if isWakeStateBoundInconclusive(err) {
					boundSelectionErr = err
				}
				return selection.Target, selection.TargetPresent, err
			},
			func(target wakeTarget) error {
				snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
				observation.Floor.Exists = exists
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("wake repair floor is missing")
				}
				fingerprint, err := newWakeCheckMetadataFingerprint(exists, snapshot.Raw, snapshot.FileInfo)
				if err != nil {
					return err
				}
				observation.Floor = fingerprint
				if err := validateWakeRepairFloor(
					snapshot.Floor,
					root,
					me,
					observation.Inspection.Lock,
					target,
				); err != nil {
					return err
				}
				return validateWakeRepairFloorCurrentBoot(snapshot.Floor)
			},
		)
		if boundSelectionErr != nil {
			return boundSelectionErr
		}
		confirmed := inspectWakeLockAt(dirfd, agentDir, root, me)
		// Catch a lock change that reverts before the outer observation comparison.
		if !sameWakeCheckInspection(observation.Inspection, confirmed) {
			return fmt.Errorf("wake state changed during inspection")
		}
		return nil
	})
	if err != nil {
		return observation, err
	}
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		return observation, err
	}
	return observation, nil
}

func recordWakeCheckSelectionTarget(observation *wakeCheckObservation, selection wakeStateReadSelection) error {
	observation.Target.Exists = selection.TargetPresent
	if !selection.TargetPresent {
		return nil
	}
	// Keep the public observation bound to legacy authority. Shadow-state
	// replacement must not create diagnostic or retry drift in P2a.
	fingerprint, err := newWakeCheckMetadataFingerprint(
		selection.legacy.TargetPresent,
		selection.legacy.Target.Raw,
		selection.legacy.Target.FileInfo,
	)
	if err != nil {
		return err
	}
	observation.Target = fingerprint
	return nil
}

func newWakeCheckMetadataFingerprint(
	exists bool,
	raw []byte,
	info os.FileInfo,
) (wakeCheckMetadataFingerprint, error) {
	fingerprint := wakeCheckMetadataFingerprint{Exists: exists}
	if !exists {
		return fingerprint, nil
	}
	identity, ok := captureWakeFileIdentity(info)
	if !ok {
		return fingerprint, fmt.Errorf("capture wake metadata file identity")
	}
	fingerprint.Identity = identity
	fingerprint.Digest = wakeMetadataDigest(raw)
	return fingerprint, nil
}

func sameWakeCheckObservation(first, second wakeCheckObservation) bool {
	return sameWakeCheckInspection(first.Inspection, second.Inspection) &&
		first.Target == second.Target &&
		first.Floor == second.Floor &&
		first.Repair == second.Repair &&
		sameWakeCheckSelfUpgradeObservation(first.SelfUpgrade, second.SelfUpgrade)
}

func opsWakeLockFromWakeCheckObservation(
	root, me string,
	observation wakeCheckObservation,
) *opsWakeLock {
	inspection := observation.Inspection
	status := string(inspection.Status)
	if !inspection.Exists {
		status = string(wakeLockMissing)
	}
	lockPath := inspection.LockPath
	if lockPath == "" {
		lockPath = filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	}
	opsLock := &opsWakeLock{
		Status:          status,
		Agent:           me,
		Root:            canonicalWakeRoot(root),
		Lock:            lockPath,
		PID:             inspection.PID,
		TTY:             strings.TrimSpace(inspection.Lock.TTY),
		Started:         strings.TrimSpace(inspection.Lock.Started),
		Reason:          inspection.Reason,
		TargetPresent:   observation.Repair.TargetPresent,
		TargetReason:    observation.Repair.TargetReason,
		RepairAvailable: observation.Repair.RepairAvailable,
		RepairReason:    observation.Repair.RepairReason,
		Repair:          observation.Repair.Repair,
		CurrentTerminal: doctorWakeLockOnCurrentTerminal(inspection),
	}
	if opsLock.TargetPresent {
		opsLock.Target = wakeTargetPath(root, me)
	}
	if isLiveRawOrphan(inspection) {
		opsLock.Status = "live-raw-orphan"
		opsLock.Reason = "live raw wake orphan; stop the owning terminal or launchd supervisor"
	}
	return opsLock
}

func unstableWakeCheckDecision(root, me string, inspection wakeLockInspection) wakeCheckDecision {
	// Unstable state cannot authorize an image conclusion, and probing again
	// would widen the observation window after instability is already known.
	decision := buildWakeCheckDecision(root, me, inspection, nil, false)
	detail := "wake state changed during inspection"
	reason := wakeRepairReasonExactEvidenceMissing
	decision.Repair.InjectViaAvailable = false
	decision.Repair.ReasonCode = &reason
	decision.Repair.Detail = &detail
	decision.Repair.legacyReason = detail
	decision.RestartCapability = wakeRestartUnavailable
	decision.Action = wakeCheckActionDecision{
		Kind:       wakeActionRetryCheck,
		Actor:      wakeActionActorAgent,
		ReasonCode: wakeReasonObservationChanged,
		Command: wakeCheckActionCommand(
			"wake", "check", "--root", decision.Root, "--me", decision.Agent,
			"--json", "--json-schema=2",
		),
		Message: "wake state changed during inspection; retry amq wake check",
	}
	decision.Reload = wakeCheckReloadDecision{
		Status:     wakeReloadUnavailable,
		ReasonCode: wakeReloadReasonObservationChanged,
	}
	finalizeWakeCheckDecision(&decision)
	return decision
}

func wakeCheckObservationFailureDecision(
	root, me string,
	inspection wakeLockInspection,
	err error,
) wakeCheckDecision {
	decision := unstableWakeCheckDecision(root, me, inspection)
	if !isWakeStateBoundInconclusive(err) {
		return decision
	}
	decision.Wake.Status = string(wakeLockUnverified)
	decision.Wake.Live = false
	decision.Repair.InjectViaAvailable = false
	return decision
}

func wakeCheckObservationFailureSnapshot(
	root, me string,
	observation wakeCheckObservation,
	err error,
) wakeCheckSnapshot {
	opsLock := opsWakeLockFromWakeCheckObservation(root, me, observation)
	if isWakeStateBoundInconclusive(err) && opsLock != nil {
		opsLock.Target = ""
		opsLock.TargetPresent = false
		opsLock.TargetReason = ""
		opsLock.Repair = ""
		opsLock.RepairAvailable = false
		opsLock.RepairReason = ""
	}
	return wakeCheckSnapshot{
		OpsLock:  opsLock,
		Decision: wakeCheckObservationFailureDecision(root, me, observation.Inspection, err),
	}
}

func isWakeStateBoundInconclusive(err error) bool {
	var bound *wakeStateBoundInconclusiveError
	return errors.As(err, &bound)
}

func sameWakeCheckInspection(first, second wakeLockInspection) bool {
	if !first.Exists || !second.Exists {
		return !first.Exists && !second.Exists
	}
	return first.Status == second.Status &&
		first.IdentityConfirmed == second.IdentityConfirmed &&
		sameWakeLockGeneration(first, second) &&
		sameWakeBinaryProcessEvidence(first.Process, second.Process)
}

func buildWakeCheckDecision(
	root, me string,
	inspection wakeLockInspection,
	opsLock *opsWakeLock,
	probeImage bool,
) wakeCheckDecision {
	// probeImage is intentionally enabled only for the single-target wake
	// check. Fleet doctor scans stay cheap and conservative: they reuse already
	// validated "different" evidence but never claim "current" from versions
	// alone.
	start := inspectWakeStartCapability(me)
	startReason, startDetail := wakeCheckStartReason(start)
	ownerBound := classifyWakeClaimForGenericTransition(inspection) == wakeClaimAuthoritative
	wakeStatus := string(inspection.Status)
	if !inspection.Exists {
		wakeStatus = string(wakeLockMissing)
	}
	decision := wakeCheckDecision{
		Agent: me,
		Root:  root,
		Platform: wakeCheckPlatformDecision{
			OS:            runtime.GOOS,
			WakeSupported: true,
		},
		Start: wakeCheckStartDecision{
			Available:  start.CanStart,
			Mode:       start.Mode,
			ReasonCode: startReason,
			Detail:     startDetail,
		},
		Wake: wakeCheckWakeDecision{
			Status:     wakeStatus,
			PID:        wakeCheckOptionalInt(inspection.PID),
			Mode:       wakeCheckOptionalString(inspection.Lock.WakeMode),
			OwnerBound: ownerBound,
		},
		Image: wakeCheckImageDecision{
			Current: wakeCheckImageEvidenceDecision{
				Path:    wakeCheckOptionalString(wakeCheckCurrentImagePath()),
				Version: wakeCheckOptionalString(wakeCheckVersion(cliVersion)),
			},
			Status: wakeImageUnknown,
		},
		Reload: classifyWakeCheckReload(root, me, inspection),
	}
	decision.Wake.Live = inspection.Exists &&
		inspection.Status == wakeLockValid &&
		inspection.IdentityConfirmed &&
		inspection.Process.Running
	if decision.Wake.Live {
		decision.Image.Running.Path = wakeCheckOptionalString(wakeCheckRunningImagePath(inspection))
		decision.Image.Running.Version = wakeCheckOptionalString(wakeCheckVersion(inspection.Lock.ImageVersion))
		if opsLock != nil && opsLock.ImageStatus == wakeImageDifferent {
			decision.Image.Status = opsLock.ImageStatus
		} else if probeImage {
			decision.Image.Status = inspectWakeCheckImageStatus(
				inspection,
				renderWakeCheckV1(decision),
			)
		}
	}

	legacyRepairReason := ""
	if opsLock != nil {
		decision.Repair.InjectViaAvailable = opsLock.RepairAvailable
		legacyRepairReason = opsLock.RepairReason
	}
	if !decision.Repair.InjectViaAvailable && legacyRepairReason == "" {
		legacyRepairReason = wakeCheckRepairIneligibility(inspection, decision.Wake.OwnerBound)
	}
	decision.Repair.legacyReason = legacyRepairReason
	decision.Repair.ReasonCode, decision.Repair.Detail = wakeCheckRepairReason(
		inspection,
		decision.Wake.OwnerBound,
		decision.Repair.InjectViaAvailable,
		legacyRepairReason,
	)
	classifyWakeCheckRestart(&decision, inspection, opsLock)
	finalizeWakeCheckDecision(&decision)
	return decision
}

func classifyWakeCheckReload(root, agent string, inspection wakeLockInspection) wakeCheckReloadDecision {
	unavailable := func(reason string) wakeCheckReloadDecision {
		return wakeCheckReloadDecision{Status: wakeReloadUnavailable, ReasonCode: reason}
	}
	if !inspection.Exists || inspection.Status != wakeLockValid ||
		!inspection.IdentityConfirmed || !inspection.Process.Running {
		return unavailable(wakeReloadReasonNotLive)
	}
	if inspection.Lock.ResumeSchema == 0 {
		return unavailable(wakeReloadReasonNotAdvertised)
	}
	if inspection.Lock.ResumeSchema != wakeResumeSchemaV2 {
		return unavailable(wakeReloadReasonSchemaUnsupported)
	}
	if err := validateWakeResumeAdvertisement(inspection.Lock, root, agent); err != nil {
		if errors.Is(err, errWakeResumeControlEndpointUnsupported) {
			return unavailable(wakeReloadReasonPlatformUnsupported)
		}
		return unavailable(wakeReloadReasonAdvertisementInvalid)
	}
	if err := validateWakeRestartTransportPlatform(inspection.Lock, root, agent); err == nil {
		readiness, err := observeWakeRestartReadiness(root, agent, inspection)
		if err != nil {
			return unavailable(wakeReloadReasonNotPrepared)
		}
		if readiness.RecordExists && readiness.Record.Status == wakeRestartPending {
			return unavailable(wakeReloadReasonRestartPending)
		}
		owner, err := wakeOwnerFromEnv()
		if err != nil || owner == nil || inspection.Lock.ResumeOwner == nil ||
			!sameWakeOwner(owner, inspection.Lock.ResumeOwner) {
			return unavailable(wakeReloadReasonOwnerMismatch)
		}
		if !readiness.Prepared {
			return unavailable(wakeReloadReasonNotPrepared)
		}
		return wakeCheckReloadDecision{
			Status:     wakeReloadReady,
			ReasonCode: wakeReloadReasonReady,
		}
	}
	return wakeCheckReloadDecision{
		Status:     wakeReloadAdvertised,
		ReasonCode: wakeReloadReasonCommandUnavailable,
	}
}

func renderWakeCheckV1(decision wakeCheckDecision) wakeCheckResult {
	return wakeCheckResult{
		Schema:                   wakeCheckSchemaV1,
		Agent:                    decision.Agent,
		Root:                     decision.Root,
		CanStartHere:             decision.Start.Available,
		StartMode:                decision.Start.Mode,
		StartReason:              wakeCheckStringValue(decision.Start.Detail, ""),
		LiveWake:                 decision.Wake.Live,
		WakeStatus:               decision.Wake.Status,
		WakePID:                  wakeCheckIntValue(decision.Wake.PID),
		WakeMode:                 wakeCheckStringValue(decision.Wake.Mode, ""),
		OwnerBound:               decision.Wake.OwnerBound,
		RunningImagePath:         wakeCheckStringValue(decision.Image.Running.Path, wakeCheckUnknown),
		RunningVersion:           wakeCheckStringValue(decision.Image.Running.Version, wakeCheckUnknown),
		CurrentImagePath:         wakeCheckStringValue(decision.Image.Current.Path, wakeCheckUnknown),
		CurrentVersion:           wakeCheckStringValue(decision.Image.Current.Version, wakeCheckUnknown),
		ImageStatus:              decision.Image.Status,
		CanRepairInjectVia:       decision.Repair.InjectViaAvailable,
		RepairReason:             decision.Repair.legacyReason,
		RestartCapability:        wakeCheckLegacyRestartCapability(decision),
		OperatorTerminalRequired: wakeCheckLegacyTerminalRequired(decision),
		NextAction:               wakeCheckLegacyActionMessage(decision),
	}
}

func wakeCheckLegacyRestartCapability(decision wakeCheckDecision) string {
	if decision.legacyRestartCapability != "" {
		return decision.legacyRestartCapability
	}
	return decision.RestartCapability
}

func wakeCheckLegacyTerminalRequired(decision wakeCheckDecision) bool {
	if decision.legacyRestartCapability != "" {
		return decision.legacyTerminalRequired
	}
	return decision.Action.TerminalRequired
}

func wakeCheckLegacyActionMessage(decision wakeCheckDecision) string {
	if decision.legacyRestartCapability != "" {
		return decision.legacyActionMessage
	}
	return decision.Action.Message
}

func wakeCheckStringValue(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func wakeCheckIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func wakeCheckRepairReason(
	inspection wakeLockInspection,
	ownerBound bool,
	available bool,
	legacyReason string,
) (*string, *string) {
	if available {
		return nil, nil
	}
	var reason string
	switch {
	case !inspection.Exists:
		reason = wakeRepairReasonNoLock
		return &reason, nil
	case ownerBound:
		reason = wakeRepairReasonOwnerBound
	case inspection.Status == wakeLockValid:
		reason = wakeRepairReasonLive
	case inspection.Status != wakeLockStale:
		reason = wakeRepairReasonNotStale
	default:
		reason = wakeRepairReasonExactEvidenceMissing
	}
	return &reason, wakeCheckOptionalString(legacyReason)
}

func decorateOpsWakeLockWithWakeCheck(
	root, agent string,
	lock *opsWakeLock,
	inspection wakeLockInspection,
	staleBinary bool,
	includeV2 bool,
) {
	root = canonicalWakeRoot(root)
	if includeV2 {
		mutation := lock.Mutation
		snapshot := inspectWakeCheckSnapshot(root, agent)
		opsLock := snapshot.OpsLock
		opsLock.Mutation = mutation
		opsLock.WakeCheckDecision = &snapshot.Decision
		*lock = *opsLock
		return
	}
	runningVersion := wakeCheckVersion(inspection.Lock.ImageVersion)
	currentVersion := wakeCheckVersion(cliVersion)
	switch {
	case runningVersion != wakeCheckUnknown &&
		currentVersion != wakeCheckUnknown &&
		runningVersion != currentVersion:
		lock.ImageStatus = wakeImageDifferent
	case staleBinary:
		lock.ImageStatus = wakeImageDifferent
	default:
		lock.ImageStatus = wakeImageUnknown
	}
	legacyDecision := buildWakeCheckDecision(root, agent, inspection, lock, false)
	check := renderWakeCheckV1(legacyDecision)
	lock.CanStartHere = check.CanStartHere
	lock.StartMode = check.StartMode
	lock.StartReason = check.StartReason
	lock.RunningImagePath = check.RunningImagePath
	lock.RunningVersion = check.RunningVersion
	lock.CurrentImagePath = check.CurrentImagePath
	lock.CurrentVersion = check.CurrentVersion
	lock.ImageStatus = check.ImageStatus
	lock.RestartCapability = check.RestartCapability
	lock.OperatorTerminalRequired = check.OperatorTerminalRequired
	lock.NextAction = check.NextAction
}

func inspectWakeStartCapability(me string) wakeStartCapability {
	mode := effectiveInjectMode(&wakeConfig{me: me, injectMode: wakeInjectModeAuto})
	if !wakeTIOCSTIAvailable() {
		return wakeStartCapability{
			Mode:   wakeInjectModeNone,
			Reason: "TIOCSTI is unavailable; a full-strength terminal wake cannot start here",
		}
	}
	if tiocstiLegacyDisabledHint() {
		return wakeStartCapability{
			Mode:   wakeInjectModeNone,
			Reason: "TIOCSTI is disabled; a full-strength terminal wake cannot start here",
		}
	}
	if !wakeInputIsTTY() {
		return wakeStartCapability{
			Mode:   mode,
			Reason: "stdin is not a TTY; start the wake from its owning terminal",
		}
	}
	return wakeStartCapability{CanStart: true, Mode: mode}
}

func classifyWakeCheckRestart(
	decision *wakeCheckDecision,
	inspection wakeLockInspection,
	opsLock *opsWakeLock,
) {
	startCommand := wakeStartCommand(decision.Root, decision.Agent)
	startArgv := wakeCheckActionCommand(
		"wake", "--root", decision.Root, "--me", decision.Agent,
	)
	retryCheckArgv := wakeCheckActionCommand(
		"wake", "check", "--root", decision.Root, "--me", decision.Agent,
		"--json", "--json-schema=2",
	)
	if !inspection.Exists {
		switch {
		case decision.Start.Available && decision.Start.Mode != wakeInjectModeNone:
			decision.RestartCapability = wakeRestartAgentSafe
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionStartWake,
				Actor:      wakeActionActorAgent,
				ReasonCode: wakeReasonMissingStartAvailable,
				Command:    startArgv,
				Message:    startCommand,
			}
		case decision.Start.Mode == wakeInjectModeNone:
			decision.RestartCapability = wakeRestartUnavailable
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionConfigureInjector,
				Actor:      wakeActionActorOperator,
				ReasonCode: wakeReasonFullStrengthUnavailable,
				Message:    "restore a supported full-strength injector or configure --inject-via; do not accept an attention-only downgrade",
			}
		default:
			decision.RestartCapability = wakeRestartOperatorOnly
			decision.Action = wakeCheckActionDecision{
				Kind:             wakeActionStartWake,
				Actor:            wakeActionActorOperator,
				ReasonCode:       wakeReasonOwningTerminalRequired,
				Command:          startArgv,
				TerminalRequired: true,
				Message:          "from the owning terminal, run " + startCommand,
			}
		}
		return
	}
	if decision.Wake.Live &&
		(decision.Reload.ReasonCode == wakeReloadReasonNotPrepared ||
			decision.Reload.ReasonCode == wakeReloadReasonRestartPending) {
		message := "leave wake state unchanged and retry after restart preparation reaches a stable state"
		if decision.Reload.ReasonCode == wakeReloadReasonRestartPending {
			message = "leave wake state unchanged and retry after the pending wake restart reaches a stable state"
		}
		decision.RestartCapability = wakeRestartUnavailable
		decision.Action = wakeCheckActionDecision{
			Kind:       wakeActionWaitForStableState,
			Actor:      wakeActionActorAgent,
			ReasonCode: decision.Reload.ReasonCode,
			Command:    retryCheckArgv,
			Message:    message,
		}
		return
	}
	if decision.Repair.InjectViaAvailable && opsLock != nil {
		decision.RestartCapability = wakeRestartAgentSafe
		decision.Action = wakeCheckActionDecision{
			Kind:       wakeActionRepairWake,
			Actor:      wakeActionActorAgent,
			ReasonCode: wakeReasonStaleRepairAvailable,
			Command: wakeCheckActionCommand(
				"wake", "repair", "--root", decision.Root, "--me", decision.Agent,
			),
			Message: opsLock.Repair,
		}
		return
	}
	if decision.Wake.Live {
		if decision.Reload.Status == wakeReloadReady {
			decision.RestartCapability = wakeRestartAgentSafe
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionRestartWake,
				Actor:      wakeActionActorAgent,
				ReasonCode: wakeReloadReasonReady,
				Command: wakeCheckActionCommand(
					"wake", "restart", "--root", decision.Root, "--me", decision.Agent,
				),
				Message: "ask the live wake to restart itself with amq wake restart",
			}
			return
		}
		decision.RestartCapability = wakeRestartOperatorOnly
		terminalRequired :=
			wakeCheckStringValue(decision.Wake.Mode, "") != wakeTargetInjectVia &&
				wakeCheckStringValue(decision.Wake.Mode, "") != wakeOwnerWakeMode
		decision.Action = wakeCheckActionDecision{
			Kind:             wakeActionPreserveLiveWake,
			Actor:            wakeActionActorOperator,
			ReasonCode:       wakeReasonLiveWakePreserve,
			TerminalRequired: terminalRequired,
			Message:          "leave the live wake running; restart it from its owning terminal or supervisor after verifying replacement readiness",
		}
		return
	}
	switch inspection.Status {
	case wakeLockStale:
		if decision.Wake.OwnerBound {
			message := wakeRecoverOwnerCommand(decision.Root, decision.Agent)
			decision.RestartCapability = wakeRestartUnavailable
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionRecoverOwner,
				Actor:      wakeActionActorOperator,
				ReasonCode: wakeReasonOwnerRecoveryRequired,
				Command: wakeCheckActionCommand(
					"wake", "recover-owner", "--root", decision.Root, "--me", decision.Agent,
				),
				Message: message,
			}
			return
		}
		if wakeCheckStringValue(decision.Wake.Mode, "") == wakeTargetInjectVia {
			decision.RestartCapability = wakeRestartUnavailable
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionConfigureInjector,
				Actor:      wakeActionActorOperator,
				ReasonCode: wakeReasonFullStrengthUnavailable,
				Message:    "restore the configured --inject-via supervisor or reconfigure a supported full-strength injector before replacing this stale wake; do not fall back to raw terminal injection",
			}
			return
		}
		if decision.Start.Mode == wakeInjectModeNone {
			decision.RestartCapability = wakeRestartUnavailable
			decision.Action = wakeCheckActionDecision{
				Kind:       wakeActionConfigureInjector,
				Actor:      wakeActionActorOperator,
				ReasonCode: wakeReasonFullStrengthUnavailable,
				Message:    "restore a supported full-strength injector or configure --inject-via; do not accept an attention-only downgrade",
			}
			return
		}
		message := fmt.Sprintf(
			"from the owning terminal, remove the proven-stale lock with %s, then run %s",
			doctorRootCommandForOS(decision.Root, "", runtime.GOOS, "--ops", "--fix-wake-locks"),
			startCommand,
		)
		decision.RestartCapability = wakeRestartOperatorOnly
		decision.Action = wakeCheckActionDecision{
			Kind:             wakeActionManualStaleCleanup,
			Actor:            wakeActionActorOperator,
			ReasonCode:       wakeReasonStaleManualCleanupRequired,
			TerminalRequired: true,
			Message:          message,
		}
	case wakeLockCreating:
		decision.RestartCapability = wakeRestartUnavailable
		decision.Action = wakeCheckActionDecision{
			Kind:       wakeActionWaitForStableState,
			Actor:      wakeActionActorAgent,
			ReasonCode: wakeReasonWakeStateCreating,
			Command:    retryCheckArgv,
			Message:    "leave wake state unchanged and retry after lock creation finishes",
		}
	default:
		decision.RestartCapability = wakeRestartUnavailable
		decision.Action = wakeCheckActionDecision{
			Kind:       wakeActionInspectUnverified,
			Actor:      wakeActionActorOperator,
			ReasonCode: wakeReasonWakeStateUnverified,
			Message:    "preserve the unverified wake state and inspect it with amq doctor --ops",
		}
	}
}

func inspectWakeCheckImageStatus(
	inspection wakeLockInspection,
	result wakeCheckResult,
) string {
	if strings.TrimSpace(inspection.Lock.ImagePath) == "" ||
		strings.TrimSpace(inspection.Lock.ImageVersion) == "" {
		return wakeImageUnknown
	}
	// A running process retains its loaded image. A recorded version mismatch is
	// therefore conclusive even if the executable path now resolves to the same
	// device and inode as the current AMQ binary.
	if result.RunningVersion != wakeCheckUnknown &&
		result.CurrentVersion != wakeCheckUnknown &&
		result.RunningVersion != result.CurrentVersion {
		return wakeImageDifferent
	}
	comparison, err := inspectWakeBinaryStaleness(inspection)
	if err != nil {
		return wakeImageUnknown
	}
	if comparison.Stale {
		return wakeImageDifferent
	}
	if comparison.Method == wakeBinaryComparisonExactIdentity ||
		comparison.Method == wakeBinaryComparisonDarwinProcessImage {
		return wakeImageCurrent
	}
	return wakeImageUnknown
}

func wakeCheckCurrentImagePath() string {
	path, resolved, err := resolveWakeExecutablePath()
	if err != nil {
		return wakeCheckUnknown
	}
	if strings.TrimSpace(resolved) != "" {
		path = resolved
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return wakeCheckUnknown
	}
	return filepath.Clean(path)
}

func wakeCheckRunningImagePath(inspection wakeLockInspection) string {
	for _, candidate := range []string{
		inspection.Lock.ImagePath,
		inspection.Process.Executable,
		inspection.Lock.Executable,
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && filepath.IsAbs(candidate) {
			return filepath.Clean(candidate)
		}
	}
	return wakeCheckUnknown
}

func wakeCheckVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return wakeCheckUnknown
	}
	return version
}

func wakeCheckRepairIneligibility(inspection wakeLockInspection, ownerBound bool) string {
	switch {
	case !inspection.Exists:
		return "no wake lock is present"
	case ownerBound:
		return "owner-bound wake state cannot use inject-via repair"
	case inspection.Status == wakeLockValid:
		return "wake is live; repair only accepts proven-stale or eligible unverified inject-via state"
	case inspection.Status != wakeLockStale:
		return "wake state is not proven repairable"
	default:
		return "no exact inject-via target and continuity floor are available"
	}
}

func wakeStartCommand(root, me string) string {
	return fmt.Sprintf(
		"amq wake --root %s --me %s",
		shellQuoteArg(root),
		shellQuoteArg(me),
	)
}

func writeWakeCheckText(result wakeCheckResult) error {
	lines := []string{
		"AMQ Wake Check",
		fmt.Sprintf("  Agent: %s", result.Agent),
		fmt.Sprintf("  Root: %s", result.Root),
		fmt.Sprintf(
			"  Direct start: %t (mode=%s reason=%s)",
			result.CanStartHere,
			result.StartMode,
			wakeCheckTextValue(result.StartReason),
		),
		fmt.Sprintf(
			"  Live wake: %t (status=%s pid=%d mode=%s owner_bound=%t)",
			result.LiveWake,
			result.WakeStatus,
			result.WakePID,
			wakeCheckTextValue(result.WakeMode),
			result.OwnerBound,
		),
		fmt.Sprintf(
			"  Running image: %s (version=%s status=%s)",
			result.RunningImagePath,
			result.RunningVersion,
			result.ImageStatus,
		),
		fmt.Sprintf(
			"  Current image: %s (version=%s)",
			result.CurrentImagePath,
			result.CurrentVersion,
		),
		fmt.Sprintf(
			"  Inject-via repair: %t (reason=%s)",
			result.CanRepairInjectVia,
			wakeCheckTextValue(result.RepairReason),
		),
		fmt.Sprintf("  Restart capability: %s", result.RestartCapability),
		fmt.Sprintf("  Next action: %s", result.NextAction),
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}
