package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/roster"
	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
	"github.com/avivsinai/agent-message-queue/internal/update"
)

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok, warn, error
	Message string `json:"message,omitempty"`
}

type doctorResult struct {
	Checks               []doctorCheck               `json:"checks"`
	Mailboxes            []fsq.MailboxInspection     `json:"mailboxes,omitempty"`
	MailboxRepair        *fsq.MailboxRepairResult    `json:"mailbox_repair,omitempty"`
	ExtensionManifests   []doctorExtensionManifest   `json:"extension_manifests,omitempty"`
	ExtensionDiagnostics []doctorExtensionDiagnostic `json:"extension_diagnostics,omitempty"`
	WakeLockSummary      *doctorWakeLockSummary      `json:"wake_locks,omitempty"`
	Summary              struct {
		OK    int `json:"ok"`
		Warn  int `json:"warn"`
		Error int `json:"error"`
	} `json:"summary"`
	Ops *doctorOpsResult `json:"ops,omitempty"`
}

func wakeCheckTextValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output as JSON")
	jsonSchemaFlag := addJSONSchemaFlag(fs)
	opsFlag := fs.Bool("ops", false, "Include runtime operational checks")
	fixWakeLocksFlag := fs.Bool("fix-wake-locks", false, "Remove stale wake lock files (implies --ops)")
	fixMailboxesFlag := fs.Bool("fix-mailboxes", false, "Create missing required directories for configured mailboxes")
	rootFlag := fs.String("root", "", "Exact AMQ root to inspect or repair")
	baseRootFlag := fs.String("base-root", "", "Config-authority root for an explicit session --root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, allow repair outside the pinned session context")
	migrateHomeFlag := fs.Bool("migrate-home", false, "Move launch trust and the keepalive home from their legacy locations into ~/.amq")
	dryRunFlag := fs.Bool("dry-run", false, "With --migrate-home, show the move without changing anything")
	yesFlag := fs.Bool("yes", false, "With --migrate-home, skip the confirmation prompt")

	usage := usageWithFlags(fs, "amq doctor [options]",
		"Verify AMQ installation and configuration.",
		"",
		"Checks:",
		"  - Binary location",
		"  - .amqrc configuration",
		"  - Mailbox directory permissions",
		"  - Agent configuration (config.json)",
		"  - Extension metadata manifests and diagnostics",
		"  - Skill installation (Claude Code / Codex / Grok Build / agy)",
		"",
		"With --ops, also checks runtime health:",
		"  - Queue depth and oldest unread per agent",
		"  - DLQ count and age",
		"  - Presence freshness",
		"  - Git worktree mailbox divergence",
		"  - Wake lock health",
		"  - Integration hints (Kanban, Symphony)",
		"",
		"With --migrate-home, move <legacy>/launch to ~/.amq/state/launch in one",
		"rename, so launches keep their trust. It refuses (exit 6) and changes",
		"nothing when an amq on PATH is older than 0.97.0 or its version cannot be",
		"read, when ~/.amq/state/launch already exists, or when the two directories",
		"are on different filesystems. <legacy> is $XDG_STATE_HOME/amq, else the",
		"user config directory (macOS, Windows), else ~/.local/state/amq.",
		"",
		"It also moves ~/.amq-keepalive to ~/.amq/keepalive, changes --registry in",
		"the supervisor plist and the AMQ SessionStart hooks that run the moved",
		"script, and bootstraps the supervisor job again. First stop the job",
		"(launchctl bootout, keep the plist), the amq-remote login service, and",
		"every amq-keepalive and amq-remote process. It refuses while one runs,",
		"while a unit is loaded or a lock is held, or when an amq, amq-keepalive or",
		"amq-remote older than 0.97.0 can run. Each part runs when its own checks",
		"pass. A rerun finishes an interrupted move.",
		"",
		"--json requires --dry-run or --yes.",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := validateJSONSchemaFlag(fs, *jsonFlag, *jsonSchemaFlag); err != nil {
		return err
	}
	if (*dryRunFlag || *yesFlag) && !*migrateHomeFlag {
		return UsageError("--dry-run and --yes require --migrate-home")
	}
	if *migrateHomeFlag {
		for _, name := range []string{"ops", "fix-wake-locks", "fix-mailboxes", "root", "base-root", "ignore-session-pin", "json-schema"} {
			if flagWasVisited(fs, name) {
				return UsageError("--migrate-home moves per-user state and cannot be combined with --%s", name)
			}
		}
		if *jsonFlag && !*dryRunFlag && !*yesFlag {
			return UsageError("--migrate-home --json requires --dry-run or --yes")
		}
		return runDoctorMigrateHome(*jsonFlag, *dryRunFlag, *yesFlag)
	}
	rootExplicit := flagWasVisited(fs, "root")
	baseRootExplicit := flagWasVisited(fs, "base-root")
	if rootExplicit && strings.TrimSpace(*rootFlag) == "" {
		return UsageError("--root cannot be empty")
	}
	if baseRootExplicit && strings.TrimSpace(*baseRootFlag) == "" {
		return UsageError("--base-root cannot be empty")
	}
	if baseRootExplicit && !rootExplicit {
		return UsageError("--base-root requires an explicit --root")
	}
	if *ignoreSessionPinFlag && !rootExplicit {
		return UsageError("--ignore-session-pin requires an explicit --root")
	}

	// --fix-wake-locks implies the ops checks it runs inside.
	opsRequested := *opsFlag || *fixWakeLocksFlag

	result := doctorResult{}

	// Check 1: Binary
	result.Checks = append(result.Checks, checkBinary())
	if check, ok := checkCodexExecutablePaths(); ok {
		result.Checks = append(result.Checks, check)
	}

	// Check 2: .amqrc
	amqrcCheck, root, amqrcSource := checkAmqrc()
	if rootExplicit {
		root = resolveRoot(*rootFlag)
		amqrcCheck = doctorCheck{
			Name:    "Root config",
			Status:  "ok",
			Message: fmt.Sprintf("queue root=%s (source: flag)", root),
		}
	}
	baseRoot := ""
	if baseRootExplicit {
		baseRoot = resolveRoot(*baseRootFlag)
	}
	if baseRoot != "" {
		if err := validateDoctorExplicitBaseRoot(root, baseRoot); err != nil {
			return UsageError("%v", err)
		}
	}
	result.Checks = append(result.Checks, amqrcCheck)

	// Check 3: Root directory
	if root != "" {
		result.Checks = append(result.Checks, checkRootDir(root))
		_, sessionPresent := os.LookupEnv(envSession)
		_, rootIDPresent := os.LookupEnv(envRootID)
		_, baseIDPresent := os.LookupEnv(envBaseRootID)
		if sessionPresent || rootIDPresent || baseIDPresent {
			result.Checks = append(result.Checks, checkSessionPinIdentity(root))
		}
	}

	// Check 4: Config.json
	if root != "" {
		configRoot := root
		if baseRoot != "" {
			configRoot = baseRoot
		}
		result.Checks = append(result.Checks, checkConfig(configRoot))
	}

	// Check 5: Mailbox permissions
	if root != "" {
		mailboxes, repair, check := inspectDoctorMailboxes(root, baseRoot, *fixMailboxesFlag, *ignoreSessionPinFlag)
		result.Mailboxes = mailboxes
		decls := roster.For(root, opsDeclarationBase(root, baseRoot))
		for i := range result.Mailboxes {
			mailbox := &result.Mailboxes[i]
			registration := roster.ClassifyAt(decls, root, mailbox.Handle).JSON()
			mailbox.RegistrationStatus = string(registration.RegistrationStatus)
			mailbox.RegistrationDiagnostics = registration.RegistrationDiagnostics
		}
		result.MailboxRepair = repair
		result.Checks = append(result.Checks, check)
	}

	// Check 6: Extension metadata
	if root != "" {
		manifests, diagnostics := scanExtensionMetadata(root)
		result.ExtensionManifests = manifests
		result.ExtensionDiagnostics = diagnostics
		result.Checks = append(result.Checks, checkExtensions(manifests, diagnostics))
	}

	// Check 7-10: Claude Code, Codex, Grok Build, and (when installed) agy skills
	result.Checks = append(result.Checks, skillChecks()...)

	// Hook config health: flag AMQ-owned SessionStart hooks whose script is gone.
	result.Checks = append(result.Checks, checkHookConfigs())
	if check, ok := checkClaudeDialogHook(); ok {
		result.Checks = append(result.Checks, check)
	}
	if check, ok := checkCodexDialogHook(); ok {
		result.Checks = append(result.Checks, check)
	}
	if check, ok := checkCursorDialogHook(); ok {
		result.Checks = append(result.Checks, check)
	}
	if check, ok := checkCodexShellEnvironment(); ok {
		result.Checks = append(result.Checks, check)
	}
	if check, ok := checkLegacyCoopHooks(); ok {
		result.Checks = append(result.Checks, check)
	}

	// Both output paths consume this one authorization decision. A refused
	// session pin cannot be reconstructed as mutation permission below.
	fixWakeLocks := *fixWakeLocksFlag
	if fixWakeLocks && root != "" {
		decision, mismatch, pinErr := doctorRepairSessionGuard(root, *ignoreSessionPinFlag)
		switch decision.Row {
		case sessionguard.RowR13DoctorInvalidErr:
			result.Checks = append(result.Checks, doctorCheck{
				Name:    "Wake lock repair",
				Status:  "error",
				Message: fmt.Sprintf("refusing to repair %s: invalid AMQ session context: %v", root, pinErr),
			})
			fixWakeLocks = false
		case sessionguard.RowR12DoctorMismatchErr:
			result.Checks = append(result.Checks, doctorCheck{
				Name:    "Wake lock repair",
				Status:  "error",
				Message: fmt.Sprintf("refusing to repair %s because it does not match the pinned session context: %s", root, mismatch.Message),
			})
			fixWakeLocks = false
		}
	}

	// Ops checks (runtime health)
	if opsRequested && root != "" {
		source := rootSourceFlag
		if !rootExplicit {
			source = amqrcSource
		}
		result.Ops = runOpsChecksWithSchema(root, string(source), fixWakeLocks, *jsonSchemaFlag, baseRoot)
	}

	// Plain and ops consume the same guarded roots and repair authorization.
	// Ops is the sole writer: a second fix traversal could remove a lock that
	// replaced one the ops mutation recheck preserved.
	if root != "" && opsRequested {
		check, projectLocks := checkProjectWakeLocks(root, baseRoot, false)
		result.Checks = append(result.Checks, check)
		result.WakeLockSummary = projectLocks
	}
	if root != "" && !opsRequested {
		check, projectLocks := checkProjectWakeLocks(root, baseRoot, fixWakeLocks)
		result.Checks = append(result.Checks, check)
		result.WakeLockSummary = projectLocks
	}

	// Calculate summary
	for _, check := range result.Checks {
		switch check.Status {
		case "ok":
			result.Summary.OK++
		case "warn":
			result.Summary.Warn++
		case "error":
			result.Summary.Error++
		}
	}

	if *jsonFlag {
		if *jsonSchemaFlag == wakeCheckSchemaV2 {
			return writeJSON(os.Stdout, renderDoctorResultV2(result))
		}
		return writeJSON(os.Stdout, result)
	}

	// Pretty print
	if err := writeStdoutLine("AMQ Doctor"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}

	statusIcons := map[string]string{"ok": "✓", "warn": "⚠", "error": "✗"}
	for _, check := range result.Checks {
		icon := statusIcons[check.Status]
		line := fmt.Sprintf("  %s %s", icon, check.Name)
		if check.Message != "" {
			line += ": " + check.Message
		}
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}

	if err := writeStdoutLine(""); err != nil {
		return err
	}

	summary := fmt.Sprintf("Summary: %d ok", result.Summary.OK)
	if result.Summary.Warn > 0 {
		summary += fmt.Sprintf(", %d warnings", result.Summary.Warn)
	}
	if result.Summary.Error > 0 {
		summary += fmt.Sprintf(", %d errors", result.Summary.Error)
	}
	if err := writeStdoutLine(summary); err != nil {
		return err
	}
	if err := writeStdoutLine("Remote control from Buzz: run `amq-remote doctor` to check its hooks, machine agent, channels and wakes."); err != nil {
		return err
	}

	// Pretty-print ops if present
	if result.Ops != nil {
		if err := writeStdoutLine(""); err != nil {
			return err
		}
		if err := writeStdoutLine("Ops:"); err != nil {
			return err
		}
		if err := writeStdoutLine(fmt.Sprintf("  Root: %s (source: %s)", result.Ops.Root.Path, result.Ops.Root.Source)); err != nil {
			return err
		}
		for _, a := range result.Ops.Agents {
			line := fmt.Sprintf("  %s: %d unread", a.Handle, a.UnreadCount)
			if a.DLQCount > 0 {
				line += fmt.Sprintf(", %d DLQ", a.DLQCount)
			}
			line += followUpRowNote(a, time.Now())
			line += fmt.Sprintf(", presence %s (%.0fs ago)", a.PresenceStatus, a.PresenceAgeSeconds)
			if a.PresenceSource != "" {
				line += ", source " + a.PresenceSource
			}
			if a.NotifierStatus != "" {
				line += fmt.Sprintf(
					", ⚠ %s mode=%s reason=%s",
					a.NotifierStatus,
					a.NotifierMode,
					a.NotifierReason,
				)
			}
			if err := writeStdoutLine(line); err != nil {
				return err
			}
		}
		if result.Ops.OperatorGate != nil {
			line := fmt.Sprintf("  operator gate: %d open", result.Ops.OperatorGate.OpenCount)
			if result.Ops.OperatorGate.OldestGateAgeSeconds > 0 {
				line += fmt.Sprintf(", oldest %.0fs", result.Ops.OperatorGate.OldestGateAgeSeconds)
			}
			if err := writeStdoutLine(line); err != nil {
				return err
			}
		}
		quarantineLine := "  wake quarantine: scan unavailable: " + result.Ops.WakeQuarantine.Error
		if result.Ops.WakeQuarantine.Error == "" {
			quarantineLine = fmt.Sprintf("  wake quarantine: %d preserved", result.Ops.WakeQuarantine.Count)
			if result.Ops.WakeQuarantine.NewestAgeSeconds != nil {
				quarantineLine += fmt.Sprintf(", newest %.0fs", *result.Ops.WakeQuarantine.NewestAgeSeconds)
			}
		}
		if err := writeStdoutLine(quarantineLine); err != nil {
			return err
		}
		for _, companion := range result.Ops.Companions {
			line := fmt.Sprintf(
				"  companion %s supervised by amq-remote up: adapter=%s target=%s state=%s",
				companion.Agent,
				companion.Adapter,
				companion.Target,
				companion.State,
			)
			if companion.LastSeen != "" {
				line += " last_seen=" + companion.LastSeen
			}
			if companion.Lock != "" {
				line += " lock=" + companion.Lock
			}
			if companion.ProbeError != "" {
				line += " probe_error=" + strings.ReplaceAll(companion.ProbeError, "\n", "; ")
			}
			if err := writeStdoutLine(line); err != nil {
				return err
			}
		}
		for _, wl := range result.Ops.WakeLocks {
			if wl.Status == string(wakeLockValid) {
				tty := wl.TTY
				if tty == "" {
					tty = "unknown"
				}
				started := wl.Started
				if started == "" {
					started = "unknown"
				}
				var line string
				if wl.CurrentTerminal {
					line = fmt.Sprintf(
						"  wake for %s is live on the current terminal: pid=%d tty=%s started=%s root=%s",
						wl.Agent,
						wl.PID,
						tty,
						started,
						wl.Root,
					)
				} else {
					line = fmt.Sprintf(
						"  wake for %s is owned by a live process: pid=%d tty=%s started=%s root=%s; "+
							"no AMQ command can safely take over this live wake",
						wl.Agent,
						wl.PID,
						tty,
						started,
						wl.Root,
					)
				}
				line += fmt.Sprintf(
					" running_image=%s running_version=%s current_image=%s current_version=%s image_status=%s restart=%s next=%s",
					wakeCheckTextValue(wl.RunningImagePath),
					wakeCheckTextValue(wl.RunningVersion),
					wakeCheckTextValue(wl.CurrentImagePath),
					wakeCheckTextValue(wl.CurrentVersion),
					wakeCheckTextValue(wl.ImageStatus),
					wakeCheckTextValue(wl.RestartCapability),
					wakeCheckTextValue(wl.NextAction),
				)
				if wl.HealthStatus != "" {
					line += fmt.Sprintf(" health=%s", wl.HealthStatus)
				}
				if wl.RestartStageStatus != "" {
					line += fmt.Sprintf(
						" restart_stage=%s restart_stage_path=%s restart_stage_reason=%s",
						wl.RestartStageStatus,
						wakeCheckTextValue(wl.RestartStagePath),
						wakeCheckTextValue(wl.RestartStageReason),
					)
				}
				if err := writeStdoutLine(line); err != nil {
					return err
				}
				continue
			}
			icon := statusIcons["warn"]
			switch wl.Status {
			case "fixed":
				icon = statusIcons["ok"]
			case "error":
				icon = statusIcons["error"]
			}
			line := fmt.Sprintf("  %s wake lock: %s agent=%s root=%s lock=%s",
				icon, wl.Status, wl.Agent, wl.Root, wl.Lock)
			if wl.PID != 0 {
				line += fmt.Sprintf(" pid=%d", wl.PID)
			}
			if wl.Reason != "" {
				line += " reason=" + wl.Reason
			}
			if wl.Fix != "" && (wl.Status == string(wakeLockStale) || wl.RestartStageStatus != "") {
				line += " fix=" + wl.Fix
			}
			if wl.TargetPresent {
				line += " target=" + wl.Target
			}
			if wl.TargetReason != "" {
				line += " target_reason=" + wl.TargetReason
			}
			if wl.RepairReason != "" {
				line += " repair_reason=" + wl.RepairReason
			}
			if wl.RepairAvailable && wl.Status == string(wakeLockStale) {
				line += " repair=" + wl.Repair
			}
			if wl.RestartCapability != "" {
				line += fmt.Sprintf(
					" running_image=%s running_version=%s current_image=%s current_version=%s image_status=%s restart=%s next=%s",
					wakeCheckTextValue(wl.RunningImagePath),
					wakeCheckTextValue(wl.RunningVersion),
					wakeCheckTextValue(wl.CurrentImagePath),
					wakeCheckTextValue(wl.CurrentVersion),
					wakeCheckTextValue(wl.ImageStatus),
					wl.RestartCapability,
					wakeCheckTextValue(wl.NextAction),
				)
			}
			if wl.RestartStageStatus != "" {
				line += fmt.Sprintf(
					" restart_stage=%s restart_stage_path=%s restart_stage_reason=%s",
					wl.RestartStageStatus,
					wakeCheckTextValue(wl.RestartStagePath),
					wakeCheckTextValue(wl.RestartStageReason),
				)
			}
			if err := writeStdoutLine(line); err != nil {
				return err
			}
		}
		for _, h := range result.Ops.Hints {
			icon := statusIcons[h.Status]
			if err := writeStdoutLine(fmt.Sprintf("  %s %s", icon, h.Message)); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateDoctorExplicitBaseRoot(root, baseRoot string) error {
	if relateTrees(root, baseRoot) == TreeRelationSame {
		return nil
	}
	if !routeWalker().IsSessionRootUnderBase(root, baseRoot) {
		return fmt.Errorf("explicit --root %s must be the base root or one direct child of --base-root %s", root, baseRoot)
	}
	baseIdentity, err := fsq.SnapshotDeliveryRoot(baseRoot)
	if err != nil {
		return fmt.Errorf("open explicit --base-root: %w", err)
	}
	baseCapability, err := fsq.OpenDeliveryRoot(baseRoot, baseIdentity)
	if err != nil {
		return fmt.Errorf("open explicit --base-root: %w", err)
	}
	defer func() { _ = baseCapability.Close() }()
	child, err := baseCapability.OpenDirectChild(filepath.Base(resolveRoot(root)))
	if err != nil {
		return fmt.Errorf("open explicit --root under --base-root: %w", err)
	}
	defer func() { _ = child.Close() }()
	return child.VerifyBase()
}

func checkSessionPinIdentity(root string) doctorCheck {
	check := doctorCheck{Name: "Session identity pin"}
	pin, err := loadSessionPin()
	if err != nil {
		check.Status = "warn"
		check.Message = err.Error()
		return check
	}
	if err := validateLegacySessionPinRoot(pin); err != nil {
		check.Status = "warn"
		check.Message = err.Error()
		return check
	}
	mismatch, err := sessionPinMismatch(root)
	if err != nil {
		check.Status = "warn"
		check.Message = err.Error()
		return check
	}
	if mismatch != nil {
		if isPinnedBaseRoot(root) {
			check.Status = "ok"
			if pin.IdentityPin {
				check.Message = "verified pinned base root"
			} else {
				check.Message = "legacy lexical pinned base root (identity tokens absent)"
			}
			return check
		}
		check.Status = "warn"
		check.Message = mismatch.Error()
		return check
	}
	if !pin.IdentityPin {
		check.Status = "ok"
		check.Message = "legacy lexical pin (identity tokens absent)"
		return check
	}
	check.Status = "ok"
	check.Message = "verified"
	return check
}

func checkBinary() doctorCheck {
	check := doctorCheck{Name: "Binary"}

	path, err := os.Executable()
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("cannot determine path: %v", err)
		return check
	}

	check.Status = "ok"
	check.Message = path
	return check
}

func checkAmqrc() (doctorCheck, string, rootSource) {
	check := doctorCheck{Name: "Root config"}

	// Use the full resolution chain including global fallback
	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		// If AM_ROOT is set, use it directly (session already resolved)
		if envVal := os.Getenv(envRoot); envVal != "" {
			check.Status = "ok"
			check.Message = fmt.Sprintf("queue root=%s (from AM_ROOT)", envVal)
			return check, envVal, ""
		}
		check.Status = "error"
		if strings.HasPrefix(err.Error(), noRootConfiguredPrefix) || strings.HasPrefix(err.Error(), noEligibleRootInGitPrefix) {
			check.Status = "warn"
		}
		check.Message = err.Error()
		return check, "", ""
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("queue root=%s (source: %s)", root, source)
	return check, root, source
}

func checkRootDir(root string) doctorCheck {
	check := doctorCheck{Name: "Root directory"}

	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		check.Status = "error"
		check.Message = fmt.Sprintf("%s does not exist", root)
		return check
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("cannot stat: %v", err)
		return check
	}
	if !info.IsDir() {
		check.Status = "error"
		check.Message = fmt.Sprintf("%s is not a directory", root)
		return check
	}

	// Check permissions (should be 0700)
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		check.Status = "warn"
		check.Message = fmt.Sprintf("%s has permissive permissions (%o)", root, perm)
		return check
	}

	check.Status = "ok"
	check.Message = root
	return check
}

// doctorWakeLockSummary is the plain-doctor view of the wake locks of one
// root and every session under it: stale counts by state and session. Lock
// files only; no process signals and no socket probe.
// doctorWakeFixResult carries one root's non-stopping repair refusal.
type doctorWakeFixResult struct {
	Removed int
	Refused string
}

type doctorWakeLockSummary struct {
	Stale      int            `json:"stale"`
	Unverified int            `json:"unverified,omitempty"`
	BySession  map[string]int `json:"stale_by_session,omitempty"`
	Refusals   []string       `json:"fix_refusals,omitempty"`
}

// doctorWakeLockSessionsUnderBase returns the session roots under base plus
// base itself, best effort. It reuses the sibling-session walk the backlog
// hints use: every direct child directory of base whose name validates as a
// session name.
func doctorWakeLockSessionsUnderBase(base string) []string {
	if strings.TrimSpace(base) == "" {
		return nil
	}
	base = absPath(resolveRoot(base))
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var roots []string
	for _, entry := range entries {
		if !entry.IsDir() || validateSessionName(entry.Name()) != nil {
			continue
		}
		// A child is a session root only when it holds a queue layout, the
		// same agents/ evidence the sibling walk requires.
		if !dirExists(filepath.Join(base, entry.Name(), "agents")) {
			continue
		}
		roots = append(roots, absPath(filepath.Join(base, entry.Name())))
	}
	return roots
}

// fixDoctorStaleWakeLocksInRoot applies the existing ops repair to one root.
// Its recheck inside the mutation scope retains generation, state-binding,
// ownership, and exact-removal checks; every unverified or structural state
// stays preserved.
func fixDoctorStaleWakeLocksInRoot(root string) doctorWakeFixResult {
	var result doctorWakeFixResult
	for _, handle := range discoveredWakeLockAgents(root, nil) {
		inspection := inspectWakeLock(root, handle)
		if !wakeLockStaleFromProcessExamination(inspection) {
			continue
		}
		if fsq.ValidateHandle(handle) != nil {
			result.Refused = fmt.Sprintf("agent %s: repair requires a valid handle", handle)
			continue
		}
		if classifyWakeClaimForGenericTransition(inspection) == wakeClaimAuthoritative {
			result.Refused = fmt.Sprintf("agent %s: owner-bound wake claims require %s", handle, wakeRecoverOwnerCommand(root, handle))
			continue
		}
		lock := opsWakeLock{}
		if err := fixStaleWakeLockForDoctor(root, handle, &inspection, &lock); err != nil {
			result.Refused = fmt.Sprintf("agent %s: %v", handle, err)
			continue
		}
		if lock.Removed {
			result.Removed++
		}
	}
	return result
}

// inspectDoctorWakeLockSummary counts the remaining locks after the shared
// guarded traversal. With fix set, each root uses the same existing repair
// helper as ops, never a wider separate removal path.
func inspectDoctorWakeLockSummary(roots map[string]string, fix bool) doctorWakeLockSummary {
	var summary doctorWakeLockSummary
	bySession := make(map[string]int)
	fixNotes := make([]string, 0)
	for root, session := range roots {
		handles := discoveredWakeLockAgents(root, nil)
		if fix {
			fixResult := fixDoctorStaleWakeLocksInRoot(root)
			if fixResult.Refused != "" && session != "" {
				fixNotes = append(fixNotes, session+": "+fixResult.Refused)
			}
		}
		for _, handle := range handles {
			inspection := inspectWakeLock(root, handle)
			if !inspection.Exists {
				continue
			}
			switch inspection.Status {
			case wakeLockStale:
				summary.Stale++
				if session != "" {
					bySession[session]++
				}
			case wakeLockUnverified:
				summary.Unverified++
			}
		}
	}
	if len(bySession) > 0 {
		summary.BySession = bySession
	}
	if len(fixNotes) > 0 {
		sort.Strings(fixNotes)
		summary.Refusals = fixNotes
	}
	return summary
}

// formatDoctorWakeLockSummary renders the plain-doctor message. Healthy roots
// get one affirmative row; stale locks name the sessions that hold them and
// the one command that removes them.
func formatDoctorWakeLockSummary(summary doctorWakeLockSummary) doctorCheck {
	check := doctorCheck{Name: "Wake locks"}
	if summary.Stale == 0 && summary.Unverified == 0 {
		check.Status = "ok"
		check.Message = "none stale"
		if len(summary.Refusals) > 0 {
			check.Status = "warn"
			check.Message = "none stale; kept: " + strings.Join(summary.Refusals, "; ")
		}
		return check
	}
	var parts []string
	if summary.Stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", summary.Stale))
	}
	if summary.Unverified > 0 {
		parts = append(parts, fmt.Sprintf("%d unverified (kept; see amq doctor --ops)", summary.Unverified))
	}
	check.Status = "warn"
	message := strings.Join(parts, "; ")
	if len(summary.BySession) > 0 {
		sessions := make([]string, 0, len(summary.BySession))
		for session := range summary.BySession {
			sessions = append(sessions, session)
		}
		slices.Sort(sessions)
		counts := make([]string, 0, len(sessions))
		for _, session := range sessions {
			counts = append(counts, fmt.Sprintf("%s: %d", session, summary.BySession[session]))
		}
		message = fmt.Sprintf("%s (%s)", message, strings.Join(counts, ", "))
	}
	if summary.Stale > 0 {
		message += "; fix: amq doctor --fix-wake-locks"
	}
	if len(summary.Refusals) > 0 {
		message += "; kept: " + strings.Join(summary.Refusals, "; ")
	}
	check.Message = message
	return check
}

// doctorWakeLockRoots is the one traversal policy shared by both output
// paths. A selected base includes its sessions. An explicit session retains
// its existing root scope; an explicit base includes that base's sessions.
func doctorWakeLockRoots(root, baseRoot string) map[string]string {
	resolvedRoot := absPath(resolveRoot(root))
	base := baseRoot
	if base != "" {
		base = absPath(resolveRoot(base))
		if base == resolvedRoot {
			return map[string]string{resolvedRoot: ""}
		}
		return map[string]string{resolvedRoot: sessionName(resolvedRoot)}
	}
	base = baseRootOfForDisplay(resolvedRoot)
	if base == resolvedRoot {
		roots := map[string]string{base: ""}
		for _, sessionRoot := range doctorWakeLockSessionsUnderBase(base) {
			roots[sessionRoot] = sessionName(sessionRoot)
		}
		return roots
	}
	return map[string]string{resolvedRoot: ""}
}

// checkProjectWakeLocksOps emits the standard per-lock row from the same
// guarded roots and fix authorization as the plain row.
func checkProjectWakeLocksOps(root, baseRoot string, fix bool, jsonSchema int) ([]opsWakeLock, []opsHint) {
	var locks []opsWakeLock
	var hints []opsHint
	for sessionRoot := range doctorWakeLockRoots(root, baseRoot) {
		rootLocks, rootHints := checkWakeLocksWithHintsSchema(
			sessionRoot,
			discoveredWakeLockAgents(sessionRoot, nil),
			fix,
			jsonSchema,
		)
		locks = append(locks, rootLocks...)
		hints = append(hints, rootHints...)
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].Lock < locks[j].Lock })
	return locks, hints
}

// checkProjectWakeLocks reads the same guarded roots as ops. With repair set,
// the plain row's helper and ops each use the existing exact removal and its
// rechecks; no structural stale or unverified lock is removed.
func checkProjectWakeLocks(root, baseRoot string, fix bool) (doctorCheck, *doctorWakeLockSummary) {
	summary := inspectDoctorWakeLockSummary(doctorWakeLockRoots(root, baseRoot), fix)
	check := formatDoctorWakeLockSummary(summary)
	return check, &summary
}

func checkConfig(root string) doctorCheck {
	check := doctorCheck{Name: "Config"}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg, err := config.LoadConfig(cfgPath)
	source := ""
	if os.IsNotExist(err) {
		// A session root without its own config uses its base's config, as
		// the mailbox check does.
		base := classifyRoot(root)
		if base == "" {
			check.Status = "warn"
			check.Message = "config.json not found"
			return check
		}
		cfg, err = config.LoadConfig(filepath.Join(base, "meta", "config.json"))
		if os.IsNotExist(err) {
			check.Status = "warn"
			check.Message = fmt.Sprintf("config.json not found in the root or its base %s", base)
			return check
		}
		source = fmt.Sprintf(" (from base %s)", base)
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("invalid: %v", err)
		return check
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("agents: %v%s", cfg.Agents, source)
	return check
}

func doctorRepairSessionGuard(root string, ignoreSessionPins bool) (sessionguard.Decision, *SessionContextError, error) {
	if ignoreSessionPins {
		return sessionguard.Decide(sessionguard.Input{
			Kind: sessionguard.KindDoctorRepair,
			Pin:  sessionguard.PinInvalid, Relation: sessionguard.TargetMismatch,
			Flags: sessionguard.Flags{ExplicitRoot: true, IgnorePin: true},
		}), nil, nil
	}
	pin, pinErr := loadSessionPin()
	if pinErr != nil {
		return sessionGuardDecisionForContext(
			sessionguard.KindDoctorRepair,
			sessionguard.ChannelExit5,
			sessionguard.PinInvalid,
			&SessionContextError{Message: pinErr.Error()},
			sessionguard.Flags{},
		), nil, pinErr
	}
	mismatch, checkErr := sessionPinMismatchWithPin(root, pin)
	if checkErr != nil {
		return sessionGuardDecisionForContext(
			sessionguard.KindDoctorRepair,
			sessionguard.ChannelExit5,
			sessionguard.PinInvalid,
			&SessionContextError{Message: checkErr.Error()},
			sessionguard.Flags{},
		), nil, checkErr
	}
	if mismatch != nil && isPinnedBaseRootWithPin(root, pin) {
		return sessionguard.Decide(sessionguard.Input{
			Kind: sessionguard.KindDoctorRepair,
			Pin:  sessionGuardPinStateFor(pin), Relation: sessionguard.TargetOwnPinnedBase,
		}), mismatch, nil
	}
	return sessionGuardDecisionForContext(
		sessionguard.KindDoctorRepair,
		sessionguard.ChannelExit5,
		sessionGuardPinStateFor(pin),
		mismatch,
		sessionguard.Flags{},
	), mismatch, nil
}

func inspectDoctorMailboxes(root, explicitBaseRoot string, repair, ignoreSessionPins bool) ([]fsq.MailboxInspection, *fsq.MailboxRepairResult, doctorCheck) {
	check := doctorCheck{Name: "Mailboxes"}
	if repair {
		decision, mismatch, pinErr := doctorRepairSessionGuard(root, ignoreSessionPins)
		if decision.Row == sessionguard.RowR13DoctorInvalidErr {
			check.Status = "error"
			check.Message = fmt.Sprintf(
				"refusing to repair %s: invalid AMQ session context: %v; re-run with a complete session context",
				root,
				pinErr,
			)
			return nil, nil, check
		}
		if decision.Row == sessionguard.RowR12DoctorMismatchErr {
			check.Status = "error"
			check.Message = fmt.Sprintf(
				"refusing to repair %s because it does not match the pinned session context: %s; re-run from the intended session",
				root,
				mismatch.Message,
			)
			return nil, nil, check
		}
	}

	var deliveryRoot, explicitConfigRoot *fsq.DeliveryRoot
	if explicitBaseRoot != "" {
		baseIdentity, err := fsq.SnapshotDeliveryRoot(explicitBaseRoot)
		if err != nil {
			check.Status = "error"
			check.Message = fmt.Sprintf("open explicit base root: %v", err)
			return nil, nil, check
		}
		explicitConfigRoot, err = fsq.OpenDeliveryRoot(explicitBaseRoot, baseIdentity)
		if err != nil {
			check.Status = "error"
			check.Message = fmt.Sprintf("open explicit base root: %v", err)
			return nil, nil, check
		}
		defer func() { _ = explicitConfigRoot.Close() }()
		if relateTrees(root, explicitBaseRoot) == TreeRelationSame {
			deliveryRoot = explicitConfigRoot
		} else {
			if !routeWalker().IsSessionRootUnderBase(root, explicitBaseRoot) {
				check.Status = "error"
				check.Message = fmt.Sprintf("explicit --root %s must be the base root or one direct child of --base-root %s", root, explicitBaseRoot)
				return nil, nil, check
			}
			deliveryRoot, err = explicitConfigRoot.OpenDirectChild(filepath.Base(resolveRoot(root)))
			if err != nil {
				check.Status = "error"
				check.Message = fmt.Sprintf("open explicit session root: %v", err)
				return nil, nil, check
			}
			defer func() { _ = deliveryRoot.Close() }()
		}
	} else {
		identity, err := fsq.SnapshotDeliveryRoot(root)
		if err != nil {
			check.Status = "error"
			check.Message = err.Error()
			return nil, nil, check
		}
		deliveryRoot, err = fsq.OpenDeliveryRoot(root, identity)
		if err != nil {
			check.Status = "error"
			check.Message = err.Error()
			return nil, nil, check
		}
		defer func() { _ = deliveryRoot.Close() }()
	}

	var (
		inventory fsq.MailboxInventory
		result    *fsq.MailboxRepairResult
	)
	configRoot := deliveryRoot
	var baseConfigRoot *fsq.DeliveryRoot
	repairBaseRoot := explicitBaseRoot
	if explicitConfigRoot != nil {
		configRoot = explicitConfigRoot
	} else if _, configErr := deliveryRoot.ReadRegularNoFollow(filepath.Join("meta", "config.json")); os.IsNotExist(configErr) {
		if base := classifyRoot(root); base != "" {
			repairBaseRoot = base
			baseIdentity, baseErr := fsq.SnapshotDeliveryRoot(base)
			if baseErr != nil {
				check.Status = "error"
				check.Message = fmt.Sprintf("open base config root: %v", baseErr)
				return nil, nil, check
			}
			baseConfigRoot, baseErr = fsq.OpenDeliveryRoot(base, baseIdentity)
			if baseErr != nil {
				check.Status = "error"
				check.Message = fmt.Sprintf("open base config root: %v", baseErr)
				return nil, nil, check
			}
			defer func() { _ = baseConfigRoot.Close() }()
			configRoot = baseConfigRoot
		}
	}

	authorization, authorizationInventory, authorizationErr := fsq.OpenMailboxConfigAuthorization(configRoot)
	if authorizationErr != nil {
		if inspected, err := fsq.InspectMailboxLayout(deliveryRoot); err == nil {
			inspected.ActiveConfigStatus = authorizationInventory.ActiveConfigStatus
			inspected.ActiveConfigIssue = authorizationInventory.ActiveConfigIssue
			inspected.RepairAuthorized = false
			authorizationInventory = inspected
		}
		if repair {
			repairResult := fsq.MailboxRepairResult{
				Status: "failed",
				Failure: &fsq.MailboxRepairFailure{
					Code:    "preflight_failed",
					Stage:   "authorization",
					Message: authorizationInventory.ActiveConfigIssue,
				},
				Inventory: authorizationInventory,
			}
			result = &repairResult
			inventory = repairResult.Inventory
			repairCommand := doctorMailboxRepairCommandForOS(root, repairBaseRoot, runtime.GOOS)
			applyDoctorMailboxRemedies(&inventory, repairCommand)
			check = checkMailboxInventory(inventory, result, repairCommand)
			return inventory.Mailboxes, result, check
		}
		inventory = authorizationInventory
		repairCommand := doctorMailboxRepairCommandForOS(root, repairBaseRoot, runtime.GOOS)
		applyDoctorMailboxRemedies(&inventory, repairCommand)
		check = checkMailboxInventory(inventory, nil, repairCommand)
		return inventory.Mailboxes, nil, check
	}
	defer func() { _ = authorization.Close() }()
	effectiveAgents := withReservedHumanHandle(authorization.ConfiguredAgents())

	if repair {
		repairResult := fsq.RepairMailboxLayoutForConfiguredAgentsWithAuthorization(
			deliveryRoot,
			authorization,
			effectiveAgents,
		)
		result = &repairResult
		inventory = repairResult.Inventory
	} else {
		inspected, err := fsq.InspectMailboxLayoutWithAuthorization(deliveryRoot, authorization, effectiveAgents...)
		if err != nil {
			check.Status = "error"
			check.Message = err.Error()
			return nil, nil, check
		}
		inventory = inspected
	}
	repairCommand := doctorMailboxRepairCommandForOS(root, repairBaseRoot, runtime.GOOS)
	applyDoctorMailboxRemedies(&inventory, repairCommand)
	check = checkMailboxInventory(inventory, result, repairCommand)
	return inventory.Mailboxes, result, check
}

func applyDoctorMailboxRemedies(inventory *fsq.MailboxInventory, repairCommand string) {
	for i := range inventory.Mailboxes {
		mailbox := &inventory.Mailboxes[i]
		if mailbox.Provenance != fsq.MailboxDiscovered || len(mailbox.Issues) == 0 {
			continue
		}
		if err := fsq.ValidateHandle(mailbox.Handle); err != nil {
			mailbox.Remedy = fmt.Sprintf(
				"preserve any messages, then rename or remove invalid entry %q under agents/",
				mailbox.Handle,
			)
			continue
		}
		mailbox.Remedy = fmt.Sprintf(
			"add %q to agents in meta/config.json, then run %s; or preserve any messages and remove agents/%s if abandoned",
			mailbox.Handle,
			repairCommand,
			mailbox.Handle,
		)
	}
}

func checkMailboxInventory(inventory fsq.MailboxInventory, repair *fsq.MailboxRepairResult, repairCommand string) doctorCheck {
	check := doctorCheck{Name: "Mailboxes", Status: "ok"}
	var issues []string
	if inventory.ActiveConfigStatus != "ok" {
		check.Status = "error"
		issues = append(issues, inventory.ActiveConfigIssue)
	}
	if inventory.AgentsIssue != "" {
		check.Status = "error"
		issues = append(issues, inventory.AgentsIssue)
	}
	for _, mailbox := range inventory.Mailboxes {
		if mailbox.Status == "error" {
			check.Status = "error"
		} else if mailbox.Status == "warn" && check.Status == "ok" {
			check.Status = "warn"
		}
		if len(mailbox.Issues) > 0 {
			detail := mailbox.Handle + ": " + strings.Join(mailbox.Issues, ", ")
			if mailbox.Remedy != "" {
				detail += "; next: " + mailbox.Remedy
			}
			issues = append(issues, detail)
		}
	}
	if repair != nil && repair.Failure != nil {
		check.Status = "error"
		issues = append(issues, "repair "+repair.Failure.Code+": "+repair.Failure.Message)
	}
	if len(inventory.Mailboxes) == 0 && len(issues) == 0 {
		check.Status = "warn"
		check.Message = "no configured or discovered mailboxes"
		return check
	}
	check.Message = fmt.Sprintf("%d mailboxes", len(inventory.Mailboxes))
	if len(issues) > 0 {
		check.Message += "; " + strings.Join(issues, "; ")
	}
	if repair == nil {
		for _, mailbox := range inventory.Mailboxes {
			if mailbox.RepairEligible {
				check.Message += "; repair: " + repairCommand
				break
			}
		}
	} else if len(repair.CreatedPaths) > 0 {
		check.Message += "; created: " + strings.Join(repair.CreatedPaths, ", ")
	}
	return check
}

// lookPathAgy reports whether the agy binary is on PATH. Tests replace it.
var lookPathAgy = func() bool {
	_, err := exec.LookPath("agy")
	return err == nil
}

// skillChecks returns one row per agent. The agy row appears only when agy is
// installed, so machines without it see no warning.
func skillChecks() []doctorCheck {
	checks := []doctorCheck{checkSkill("claude"), checkSkill("codex"), checkSkill("grok")}
	if lookPathAgy() {
		checks = append(checks, checkSkill("agy"))
	}
	return checks
}

func checkSkill(agent string) doctorCheck {
	check := doctorCheck{Name: fmt.Sprintf("%s skill", agent)}

	switch agent {
	case "claude", "codex", "grok", "agy":
	default:
		check.Status = "warn"
		check.Message = "unknown agent"
		return check
	}

	home, _ := os.UserHomeDir()

	// Candidate skill directories, in priority order. The shared ~/.agents/skills
	// (and project-local .agents/skills) is where `npx skills add -g` installs,
	// so every agent checks it in addition to its agent-specific dir. The legacy
	// ~/.<agent>/skills paths remain for installs created before the shared dir.
	agentDir := "." + agent
	if agent == "agy" {
		// agy reads project .agents/skills and ~/.gemini/config/skills only;
		// it never reads ~/.agy or ~/.agents.
		candidates := []string{
			filepath.Join(".agents", "skills", "amq-cli"),
			filepath.Join(home, ".gemini", "config", "skills", "amq-cli"),
		}
		return skillCandidatesResult(check, candidates,
			"mkdir -p ~/.gemini/config/skills && cp -R <amq clone>/skills/amq-cli ~/.gemini/config/skills/")
	}
	candidates := []string{
		filepath.Join(agentDir, "skills", "amq-cli"),        // project-local, agent-specific
		filepath.Join(".agents", "skills", "amq-cli"),       // project-local, shared
		filepath.Join(home, agentDir, "skills", "amq-cli"),  // user-level, agent-specific
		filepath.Join(home, ".agents", "skills", "amq-cli"), // user-level, shared (npx skills add -g)
	}

	return skillCandidatesResult(check, candidates,
		"npx skills add avivsinai/agent-message-queue -g -y")
}

// skillVersion is the version in a SKILL.md frontmatter, or "" when the
// file has no frontmatter or no version line.
func skillVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return ""
	}
	contents := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(contents, "\n")
	if lines[0] != "---" {
		return ""
	}
	frontmatter := lines[1:]
	end := slices.Index(frontmatter, "---")
	if end == -1 {
		return ""
	}
	for _, line := range frontmatter[:end] {
		value, ok := strings.CutPrefix(line, "version:")
		if !ok {
			continue
		}
		value, _, _ = strings.Cut(value, "#")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		return value
	}
	return ""
}

// skillCandidatesResult uses the first candidate that holds a SKILL.md and
// warns if its version is older than the binary. With no file it warns of a
// partial install or gives the install command.
func skillCandidatesResult(check doctorCheck, candidates []string, install string) doctorCheck {
	// Check project-local candidates first, then user-level, in order.
	for _, dir := range candidates {
		path := filepath.Join(dir, "SKILL.md")
		if fileExists(path) {
			local := strings.HasPrefix(dir, ".")
			v := skillVersion(path)
			if cmp, ok := update.CompareVersions(v, cliVersion); ok && cmp < 0 {
				check.Status = "warn"
				if local {
					check.Message = fmt.Sprintf("installed (project-local), but its version %s is older than amq %s; update the project's copy, then restart the agent", v, cliVersion)
				} else {
					check.Message = fmt.Sprintf("installed, but its version %s is older than amq %s (run: %s, then restart the agent)", v, cliVersion, install)
				}
				return check
			}
			check.Status = "ok"
			if local {
				check.Message = "installed (project-local)"
			} else {
				check.Message = "installed"
			}
			return check
		}
	}

	// No SKILL.md in any candidate. Warn if a skill directory exists but is
	// missing SKILL.md (partial install); otherwise recommend the remedy.
	for _, dir := range candidates {
		if dirExists(dir) {
			check.Status = "warn"
			check.Message = "skill directory exists but SKILL.md missing"
			return check
		}
	}

	check.Status = "warn"
	check.Message = "not installed (run: " + install + ")"
	return check
}
