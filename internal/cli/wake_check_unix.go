//go:build darwin || linux

package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	wakeCheckSchema = 1

	wakeRestartAgentSafe    = "agent_safe"
	wakeRestartOperatorOnly = "operator_only"
	wakeRestartUnavailable  = "unavailable"

	wakeImageCurrent   = "current"
	wakeImageDifferent = "different"
	wakeImageUnknown   = "unknown"

	wakeCheckUnknown = "unknown"
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

func runWakeCheck(args []string) error {
	fs := flag.NewFlagSet("wake check", flag.ContinueOnError)
	common := addCommonFlags(fs)
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

	result := inspectWakeCheck(root, me)
	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	return writeWakeCheckText(result)
}

func inspectWakeCheck(root, me string) wakeCheckResult {
	root = canonicalWakeRoot(root)
	before := inspectWakeLock(root, me)
	checked := checkWakeLocks(root, []string{me}, false)
	inspection := inspectWakeLock(root, me)
	var opsLock *opsWakeLock
	if sameWakeCheckInspection(before, inspection) &&
		inspection.Exists &&
		len(checked) == 1 &&
		checked[0].Agent == me {
		opsLock = &checked[0]
	}
	result := buildWakeCheckResult(root, me, inspection, opsLock, true)
	after := inspectWakeLock(root, me)
	stable := sameWakeCheckInspection(before, inspection) &&
		sameWakeCheckInspection(inspection, after)
	if !stable {
		result.CanRepairInjectVia = false
		result.RepairReason = "wake state changed during inspection"
		result.RestartCapability = wakeRestartUnavailable
		result.OperatorTerminalRequired = false
		result.NextAction = "wake state changed during inspection; retry amq wake check"
	}
	return result
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

func buildWakeCheckResult(
	root, me string,
	inspection wakeLockInspection,
	opsLock *opsWakeLock,
	probeImage bool,
) wakeCheckResult {
	// probeImage is intentionally enabled only for the single-target wake
	// check. Fleet doctor scans stay cheap and conservative: they reuse already
	// validated "different" evidence but never claim "current" from versions
	// alone.
	start := inspectWakeStartCapability(me)
	result := wakeCheckResult{
		Schema:           wakeCheckSchema,
		Agent:            me,
		Root:             root,
		CanStartHere:     start.CanStart,
		StartMode:        start.Mode,
		StartReason:      start.Reason,
		WakeStatus:       string(inspection.Status),
		WakePID:          inspection.PID,
		WakeMode:         strings.TrimSpace(inspection.Lock.WakeMode),
		OwnerBound:       classifyWakeClaimForGenericTransition(inspection) == wakeClaimAuthoritative,
		RunningImagePath: wakeCheckUnknown,
		RunningVersion:   wakeCheckUnknown,
		CurrentImagePath: wakeCheckCurrentImagePath(),
		CurrentVersion:   wakeCheckVersion(cliVersion),
		ImageStatus:      wakeImageUnknown,
	}
	if !inspection.Exists {
		result.WakeStatus = string(wakeLockMissing)
	}
	result.LiveWake = inspection.Exists &&
		inspection.Status == wakeLockValid &&
		inspection.IdentityConfirmed &&
		inspection.Process.Running
	if result.LiveWake {
		result.RunningImagePath = wakeCheckRunningImagePath(inspection)
		result.RunningVersion = wakeCheckVersion(inspection.Lock.ImageVersion)
		if opsLock != nil && opsLock.ImageStatus == wakeImageDifferent {
			result.ImageStatus = opsLock.ImageStatus
		} else if probeImage {
			result.ImageStatus = inspectWakeCheckImageStatus(inspection, result)
		}
	}

	if opsLock != nil {
		result.CanRepairInjectVia = opsLock.RepairAvailable
		result.RepairReason = opsLock.RepairReason
	}
	if !result.CanRepairInjectVia && result.RepairReason == "" {
		result.RepairReason = wakeCheckRepairIneligibility(inspection, result.OwnerBound)
	}
	classifyWakeCheckRestart(&result, inspection, opsLock)
	return result
}

func decorateOpsWakeLockWithWakeCheck(
	root string,
	lock *opsWakeLock,
	inspection wakeLockInspection,
	staleBinary bool,
) {
	root = canonicalWakeRoot(root)
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
	check := buildWakeCheckResult(root, lock.Agent, inspection, lock, false)
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
	result *wakeCheckResult,
	inspection wakeLockInspection,
	opsLock *opsWakeLock,
) {
	startCommand := wakeStartCommand(result.Root, result.Agent)
	if !inspection.Exists {
		switch {
		case result.CanStartHere && result.StartMode != wakeInjectModeNone:
			result.RestartCapability = wakeRestartAgentSafe
			result.NextAction = startCommand
		case result.StartMode == wakeInjectModeNone:
			result.RestartCapability = wakeRestartUnavailable
			result.NextAction = "restore a supported full-strength injector or configure --inject-via; do not accept an attention-only downgrade"
		default:
			result.RestartCapability = wakeRestartOperatorOnly
			result.OperatorTerminalRequired = true
			result.NextAction = "from the owning terminal, run " + startCommand
		}
		return
	}
	if result.CanRepairInjectVia && opsLock != nil {
		result.RestartCapability = wakeRestartAgentSafe
		result.NextAction = opsLock.Repair
		return
	}
	if result.LiveWake {
		result.RestartCapability = wakeRestartOperatorOnly
		result.OperatorTerminalRequired =
			result.WakeMode != wakeTargetInjectVia &&
				result.WakeMode != wakeOwnerWakeMode
		result.NextAction = "leave the live wake running; restart it from its owning terminal or supervisor after verifying replacement readiness"
		return
	}
	switch inspection.Status {
	case wakeLockStale:
		if result.OwnerBound {
			result.RestartCapability = wakeRestartUnavailable
			result.NextAction = wakeRecoverOwnerCommand(result.Root, result.Agent)
			return
		}
		result.RestartCapability = wakeRestartOperatorOnly
		result.OperatorTerminalRequired = true
		result.NextAction = fmt.Sprintf(
			"from the owning terminal, remove the proven-stale lock with %s, then run %s",
			doctorRootCommandForOS(result.Root, "", runtime.GOOS, "--ops", "--fix-wake-locks"),
			startCommand,
		)
	case wakeLockCreating:
		result.RestartCapability = wakeRestartUnavailable
		result.NextAction = "leave wake state unchanged and retry after lock creation finishes"
	default:
		result.RestartCapability = wakeRestartUnavailable
		result.NextAction = "preserve the unverified wake state and inspect it with amq doctor --ops"
	}
}

func inspectWakeCheckImageStatus(
	inspection wakeLockInspection,
	result wakeCheckResult,
) string {
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
		(result.RunningVersion != wakeCheckUnknown &&
			result.CurrentVersion != wakeCheckUnknown &&
			result.RunningVersion == result.CurrentVersion) {
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
