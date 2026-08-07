package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	wakeCheckSchemaV1 = 1
	wakeCheckSchemaV2 = 2

	wakeRestartAgentSafe    = "agent_safe"
	wakeRestartOperatorOnly = "operator_only"
	wakeRestartUnavailable  = "unavailable"

	wakeImageCurrent   = "current"
	wakeImageDifferent = "different"
	wakeImageUnknown   = "unknown"

	wakeCheckUnknown      = "unknown"
	wakeReloadAdvertised  = "advertised"
	wakeReloadReady       = "ready"
	wakeReloadUnavailable = "unavailable"
)

const (
	wakeActionStartWake          = "start_wake"
	wakeActionRepairWake         = "repair_wake"
	wakeActionRestartWake        = "restart_wake"
	wakeActionRecoverOwner       = "recover_owner"
	wakeActionPreserveLiveWake   = "preserve_live_wake"
	wakeActionInspectUnverified  = "inspect_unverified"
	wakeActionRetryCheck         = "retry_check"
	wakeActionConfigureInjector  = "configure_injector"
	wakeActionManualStaleCleanup = "manual_stale_cleanup"
	wakeActionWaitForStableState = "wait_for_stable_state"
	wakeActionUnsupported        = "unsupported"

	wakeActionActorAgent    = "agent"
	wakeActionActorOperator = "operator"
	wakeActionActorNone     = "none"
)

const (
	wakeReasonMissingStartAvailable      = "wake_missing_start_available"
	wakeReasonFullStrengthUnavailable    = "full_strength_injector_unavailable"
	wakeReasonOwningTerminalRequired     = "owning_terminal_required"
	wakeReasonStaleRepairAvailable       = "stale_inject_via_repair_available"
	wakeReasonLiveWakePreserve           = "live_wake_preserve"
	wakeReasonOwnerRecoveryRequired      = "owner_bound_stale_recovery_required"
	wakeReasonStaleManualCleanupRequired = "stale_lock_manual_cleanup_required"
	wakeReasonWakeStateCreating          = "wake_state_creating"
	wakeReasonWakeStateUnverified        = "wake_state_unverified"
	wakeReasonObservationChanged         = "observation_changed"
	wakeReasonExecutableUnavailable      = "executable_identity_unavailable"
	wakeReasonPlatformUnsupported        = "platform_unsupported"
	wakeRepairReasonNoLock               = "no_wake_lock"
	wakeRepairReasonOwnerBound           = "owner_bound"
	wakeRepairReasonLive                 = "wake_live"
	wakeRepairReasonNotStale             = "wake_not_stale"
	wakeRepairReasonExactEvidenceMissing = "exact_repair_evidence_unavailable"
	wakeReloadReasonNotLive              = "reload_not_live"
	wakeReloadReasonNotAdvertised        = "reload_not_advertised"
	wakeReloadReasonSchemaUnsupported    = "reload_schema_unsupported"
	wakeReloadReasonAdvertisementInvalid = "reload_advertisement_invalid"
	wakeReloadReasonObservationChanged   = "reload_observation_changed"
	wakeReloadReasonPlatformUnsupported  = "reload_platform_unsupported"
	wakeReloadReasonCommandUnavailable   = "reload_command_unavailable"
	wakeReloadReasonReady                = "reload_ready"
	wakeReloadReasonOwnerMismatch        = "reload_owner_mismatch"
	wakeReloadReasonNotPrepared          = "reload_not_prepared"
	wakeReloadReasonRestartPending       = "reload_restart_pending"
)

type wakeCheckDecision struct {
	Agent                   string
	Root                    string
	Platform                wakeCheckPlatformDecision
	Start                   wakeCheckStartDecision
	Wake                    wakeCheckWakeDecision
	Image                   wakeCheckImageDecision
	Repair                  wakeCheckRepairDecision
	Reload                  wakeCheckReloadDecision
	SelfUpgrade             wakeCheckSelfUpgradeDecision
	RestartCapability       string
	Action                  wakeCheckActionDecision
	legacyRestartCapability string
	legacyTerminalRequired  bool
	legacyActionMessage     string
}

type wakeCheckPlatformDecision struct {
	OS            string
	WakeSupported bool
	ReasonCode    *string
}

type wakeCheckStartDecision struct {
	Available  bool
	Mode       string
	ReasonCode *string
	Detail     *string
}

type wakeCheckWakeDecision struct {
	Status     string
	Live       bool
	PID        *int
	Mode       *string
	OwnerBound bool
}

type wakeCheckImageDecision struct {
	Running wakeCheckImageEvidenceDecision
	Current wakeCheckImageEvidenceDecision
	Status  string
}

type wakeCheckImageEvidenceDecision struct {
	Path    *string
	Version *string
}

type wakeCheckRepairDecision struct {
	InjectViaAvailable bool
	ReasonCode         *string
	Detail             *string
	legacyReason       string
}

type wakeCheckReloadDecision struct {
	Status     string
	ReasonCode string
}

// wakeCheckSelfUpgradeDecision is diagnostic-only state for a wake's optional
// self-upgrade observer. Its zero value deliberately renders as an explicit,
// disabled schema-v2 object so callers never need to infer field absence.
type wakeCheckSelfUpgradeDecision struct {
	Enabled       bool
	Eligible      bool
	Locator       *string
	LastCandidate *wakeCheckSelfUpgradeCandidateDecision
	LastDecision  *wakeCheckSelfUpgradeLastDecision
	RefusedMemory bool
}

type wakeCheckSelfUpgradeCandidateDecision struct {
	Identity string
	Version  string
}

type wakeCheckSelfUpgradeLastDecision struct {
	Action string
	Reason string
	At     string
}

type wakeCheckActionDecision struct {
	Kind             string
	Actor            string
	ReasonCode       string
	Command          *wakeCheckCommand
	TerminalRequired bool
	Message          string
}

type wakeCheckCommand struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
}

type wakeCheckResultV2 struct {
	Schema            int                    `json:"schema"`
	Agent             string                 `json:"agent"`
	Root              string                 `json:"root"`
	Platform          wakeCheckPlatformV2    `json:"platform"`
	Start             wakeCheckStartV2       `json:"start"`
	Wake              wakeCheckWakeV2        `json:"wake"`
	Image             wakeCheckImageV2       `json:"image"`
	Repair            wakeCheckRepairV2      `json:"repair"`
	Reload            wakeCheckReloadV2      `json:"reload"`
	SelfUpgrade       wakeCheckSelfUpgradeV2 `json:"self_upgrade"`
	RestartCapability string                 `json:"restart_capability"`
	Action            wakeCheckActionV2      `json:"action"`
}

type wakeCheckPlatformV2 struct {
	OS            string  `json:"os"`
	WakeSupported bool    `json:"wake_supported"`
	ReasonCode    *string `json:"reason_code"`
}

type wakeCheckStartV2 struct {
	Available  bool    `json:"available"`
	Mode       string  `json:"mode"`
	ReasonCode *string `json:"reason_code"`
	Detail     *string `json:"detail"`
}

type wakeCheckWakeV2 struct {
	Status     string  `json:"status"`
	Live       bool    `json:"live"`
	PID        *int    `json:"pid"`
	Mode       *string `json:"mode"`
	OwnerBound bool    `json:"owner_bound"`
}

type wakeCheckImageV2 struct {
	Running wakeCheckImageEvidenceV2 `json:"running"`
	Current wakeCheckImageEvidenceV2 `json:"current"`
	Status  string                   `json:"status"`
}

type wakeCheckImageEvidenceV2 struct {
	Path    *string `json:"path"`
	Version *string `json:"version"`
}

type wakeCheckRepairV2 struct {
	InjectViaAvailable bool    `json:"inject_via_available"`
	ReasonCode         *string `json:"reason_code"`
	Detail             *string `json:"detail"`
}

type wakeCheckReloadV2 struct {
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
}

type wakeCheckSelfUpgradeV2 struct {
	Enabled       bool                                `json:"enabled"`
	Eligible      bool                                `json:"eligible"`
	Locator       *string                             `json:"locator"`
	LastCandidate *wakeCheckSelfUpgradeCandidateV2    `json:"last_candidate"`
	LastDecision  *wakeCheckSelfUpgradeLastDecisionV2 `json:"last_decision"`
	RefusedMemory bool                                `json:"refused_memory"`
}

type wakeCheckSelfUpgradeCandidateV2 struct {
	Identity string `json:"identity"`
	Version  string `json:"version"`
}

type wakeCheckSelfUpgradeLastDecisionV2 struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type wakeCheckActionV2 struct {
	Kind             string            `json:"kind"`
	Actor            string            `json:"actor"`
	ReasonCode       string            `json:"reason_code"`
	Command          *wakeCheckCommand `json:"command"`
	TerminalRequired bool              `json:"terminal_required"`
	Message          string            `json:"message"`
}

var wakeCheckExecutable = os.Executable

func renderWakeCheckV2(decision wakeCheckDecision) wakeCheckResultV2 {
	return wakeCheckResultV2{
		Schema: wakeCheckSchemaV2,
		Agent:  decision.Agent,
		Root:   decision.Root,
		Platform: wakeCheckPlatformV2{
			OS:            decision.Platform.OS,
			WakeSupported: decision.Platform.WakeSupported,
			ReasonCode:    decision.Platform.ReasonCode,
		},
		Start: wakeCheckStartV2{
			Available:  decision.Start.Available,
			Mode:       decision.Start.Mode,
			ReasonCode: decision.Start.ReasonCode,
			Detail:     decision.Start.Detail,
		},
		Wake: wakeCheckWakeV2{
			Status:     decision.Wake.Status,
			Live:       decision.Wake.Live,
			PID:        decision.Wake.PID,
			Mode:       decision.Wake.Mode,
			OwnerBound: decision.Wake.OwnerBound,
		},
		Image: wakeCheckImageV2{
			Running: wakeCheckImageEvidenceV2(decision.Image.Running),
			Current: wakeCheckImageEvidenceV2(decision.Image.Current),
			Status:  decision.Image.Status,
		},
		Repair: wakeCheckRepairV2{
			InjectViaAvailable: decision.Repair.InjectViaAvailable,
			ReasonCode:         decision.Repair.ReasonCode,
			Detail:             decision.Repair.Detail,
		},
		Reload: wakeCheckReloadV2{
			Status:     decision.Reload.Status,
			ReasonCode: decision.Reload.ReasonCode,
		},
		SelfUpgrade:       renderWakeCheckSelfUpgradeV2(decision.SelfUpgrade),
		RestartCapability: decision.RestartCapability,
		Action: wakeCheckActionV2{
			Kind:             decision.Action.Kind,
			Actor:            decision.Action.Actor,
			ReasonCode:       decision.Action.ReasonCode,
			Command:          decision.Action.Command,
			TerminalRequired: decision.Action.TerminalRequired,
			Message:          decision.Action.Message,
		},
	}
}

func renderWakeCheckSelfUpgradeV2(decision wakeCheckSelfUpgradeDecision) wakeCheckSelfUpgradeV2 {
	result := wakeCheckSelfUpgradeV2{
		Enabled:       decision.Enabled,
		Eligible:      decision.Eligible,
		Locator:       decision.Locator,
		RefusedMemory: decision.RefusedMemory,
	}
	if decision.LastCandidate != nil {
		result.LastCandidate = &wakeCheckSelfUpgradeCandidateV2{
			Identity: decision.LastCandidate.Identity,
			Version:  decision.LastCandidate.Version,
		}
	}
	if decision.LastDecision != nil {
		result.LastDecision = &wakeCheckSelfUpgradeLastDecisionV2{
			Action: decision.LastDecision.Action,
			Reason: decision.LastDecision.Reason,
			At:     decision.LastDecision.At,
		}
	}
	return result
}

func addJSONSchemaFlag(fs *flag.FlagSet) *int {
	return fs.Int("json-schema", wakeCheckSchemaV1, "JSON schema version: 1 or 2 (requires --json)")
}

func validateJSONSchemaFlag(fs *flag.FlagSet, jsonOutput bool, schema int) error {
	if flagWasVisited(fs, "json-schema") && !jsonOutput {
		return UsageError("--json-schema requires --json")
	}
	if schema != wakeCheckSchemaV1 && schema != wakeCheckSchemaV2 {
		return UsageError("--json-schema must be 1 or 2")
	}
	return nil
}

func wakeCheckV2OptInPresent(args []string) bool {
	fs := flag.NewFlagSet("wake check v2 opt-in", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "")
	jsonSchema := fs.Int("json-schema", wakeCheckSchemaV1, "")
	_ = fs.String("root", "", "")
	_ = fs.String("me", "", "")
	_ = fs.Bool("strict", false, "")
	if err := fs.Parse(args); err != nil {
		return false
	}
	return *jsonOutput && *jsonSchema == wakeCheckSchemaV2
}

func wakeCheckActionProgram() string {
	path, err := wakeCheckExecutable()
	if err != nil {
		return ""
	}
	if path == "" || strings.TrimSpace(path) != path || strings.ContainsRune(path, 0) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ""
	}
	return path
}

func wakeCheckActionCommand(args ...string) *wakeCheckCommand {
	program := wakeCheckActionProgram()
	if program == "" {
		return nil
	}
	return &wakeCheckCommand{Program: program, Args: append([]string(nil), args...)}
}

func finalizeWakeCheckDecision(decision *wakeCheckDecision) {
	decision.legacyRestartCapability = decision.RestartCapability
	decision.legacyTerminalRequired = decision.Action.TerminalRequired
	decision.legacyActionMessage = decision.Action.Message
	if !wakeCheckActionRequiresExecutable(decision.Action) || decision.Action.Command != nil {
		return
	}
	decision.RestartCapability = wakeRestartUnavailable
	decision.Action = wakeCheckActionDecision{
		Kind:       wakeActionRetryCheck,
		Actor:      wakeActionActorAgent,
		ReasonCode: wakeReasonExecutableUnavailable,
		Message:    "rerun amq wake check from a valid installed AMQ image; executable identity is unavailable",
	}
}

func wakeCheckActionRequiresExecutable(action wakeCheckActionDecision) bool {
	switch action.Kind {
	case wakeActionStartWake, wakeActionRepairWake, wakeActionRestartWake, wakeActionRecoverOwner,
		wakeActionWaitForStableState:
		return true
	case wakeActionRetryCheck:
		return action.ReasonCode == wakeReasonObservationChanged
	default:
		return false
	}
}

func wakeCheckOptionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" || value == wakeCheckUnknown {
		return nil
	}
	return &value
}

func wakeCheckOptionalInt(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}

func unsupportedWakeCheckDecision(root, agent string) wakeCheckDecision {
	reason := wakeReasonPlatformUnsupported
	detail := "amq wake is not supported on this platform"
	return wakeCheckDecision{
		Agent: agent,
		Root:  canonicalWakeRoot(root),
		Platform: wakeCheckPlatformDecision{
			OS:            runtime.GOOS,
			WakeSupported: false,
			ReasonCode:    &reason,
		},
		Start: wakeCheckStartDecision{
			Available:  false,
			Mode:       wakeInjectModeNone,
			ReasonCode: &reason,
			Detail:     &detail,
		},
		Wake: wakeCheckWakeDecision{
			Status: string(wakeLockMissing),
		},
		Image: wakeCheckImageDecision{
			Current: wakeCheckImageEvidenceDecision{
				Path:    wakeCheckOptionalString(wakeCheckActionProgram()),
				Version: wakeCheckOptionalString(cliVersion),
			},
			Status: wakeImageUnknown,
		},
		Repair: wakeCheckRepairDecision{
			ReasonCode:   &reason,
			Detail:       &detail,
			legacyReason: detail,
		},
		Reload: wakeCheckReloadDecision{
			Status:     wakeReloadUnavailable,
			ReasonCode: wakeReloadReasonPlatformUnsupported,
		},
		RestartCapability: wakeRestartUnavailable,
		Action: wakeCheckActionDecision{
			Kind:       wakeActionUnsupported,
			Actor:      wakeActionActorNone,
			ReasonCode: reason,
			Message:    detail,
		},
	}
}
