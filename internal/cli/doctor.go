package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	Summary              struct {
		OK    int `json:"ok"`
		Warn  int `json:"warn"`
		Error int `json:"error"`
	} `json:"summary"`
	Ops *doctorOpsResult `json:"ops,omitempty"`
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output as JSON")
	opsFlag := fs.Bool("ops", false, "Include runtime operational checks")
	fixWakeLocksFlag := fs.Bool("fix-wake-locks", false, "With --ops, remove stale wake lock files")
	fixMailboxesFlag := fs.Bool("fix-mailboxes", false, "Create missing required directories for configured mailboxes")
	rootFlag := fs.String("root", "", "Exact AMQ root to inspect or repair")
	baseRootFlag := fs.String("base-root", "", "Config-authority root for an explicit session --root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, allow repair outside the pinned session context")

	usage := usageWithFlags(fs, "amq doctor [options]",
		"Verify AMQ installation and configuration.",
		"",
		"Checks:",
		"  - Binary location",
		"  - .amqrc configuration",
		"  - Mailbox directory permissions",
		"  - Agent configuration (config.json)",
		"  - Extension metadata manifests and diagnostics",
		"  - Skill installation (Claude Code / Codex)",
		"",
		"With --ops, also checks runtime health:",
		"  - Queue depth and oldest unread per agent",
		"  - DLQ count and age",
		"  - Presence freshness",
		"  - Git worktree mailbox divergence",
		"  - Wake lock health",
		"  - Integration hints (Kanban, Symphony)",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *fixWakeLocksFlag && !*opsFlag {
		return UsageError("--fix-wake-locks requires --ops")
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

	result := doctorResult{}

	// Check 1: Binary
	result.Checks = append(result.Checks, checkBinary())

	// Check 2: .amqrc
	amqrcCheck, root := checkAmqrc()
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

	// Check 7: Claude Code skill
	result.Checks = append(result.Checks, checkSkill("claude"))

	// Check 8: Codex skill
	result.Checks = append(result.Checks, checkSkill("codex"))

	// Ops checks (runtime health)
	if *opsFlag && root != "" {
		// Resolve root source once here; avoids re-resolving with empty flags in runOpsChecks.
		source := rootSourceFlag
		if !rootExplicit {
			_, source, _, _ = resolveEnvConfigWithSource("", "")
		}
		fixWakeLocks := *fixWakeLocksFlag
		if fixWakeLocks && !*ignoreSessionPinFlag {
			mismatch, pinErr := sessionPinMismatch(root)
			if pinErr != nil {
				result.Checks = append(result.Checks, doctorCheck{
					Name:    "Wake lock repair",
					Status:  "error",
					Message: fmt.Sprintf("refusing to repair %s: invalid AMQ session context: %v", root, pinErr),
				})
				fixWakeLocks = false
			} else if mismatch != nil && !isPinnedBaseRoot(root) {
				result.Checks = append(result.Checks, doctorCheck{
					Name:    "Wake lock repair",
					Status:  "error",
					Message: fmt.Sprintf("refusing to repair %s because it does not match the pinned session context: %s", root, mismatch.Message),
				})
				fixWakeLocks = false
			}
		}
		result.Ops = runOpsChecks(root, string(source), fixWakeLocks, baseRoot)
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
		for _, wl := range result.Ops.WakeLocks {
			if wl.Status == string(wakeLockValid) {
				if wl.CurrentTerminal {
					continue
				}
				tty := wl.TTY
				if tty == "" {
					tty = "unknown"
				}
				started := wl.Started
				if started == "" {
					started = "unknown"
				}
				line := fmt.Sprintf(
					"  wake for %s is owned by a live process: pid=%d tty=%s started=%s root=%s; "+
						"no AMQ command can safely take over this live wake",
					wl.Agent,
					wl.PID,
					tty,
					started,
					wl.Root,
				)
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
			if wl.Fix != "" && wl.Status == string(wakeLockStale) {
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
	if !isSessionRootUnderBase(root, baseRoot) {
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

func checkAmqrc() (doctorCheck, string) {
	check := doctorCheck{Name: "Root config"}

	// Use the full resolution chain including global fallback
	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		// If AM_ROOT is set, use it directly (session already resolved)
		if envVal := os.Getenv(envRoot); envVal != "" {
			check.Status = "ok"
			check.Message = fmt.Sprintf("queue root=%s (from AM_ROOT)", envVal)
			return check, envVal
		}
		check.Status = "warn"
		check.Message = "no root found (run 'amq coop init' or create ~/.amqrc)"
		return check, ""
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("queue root=%s (source: %s)", root, source)
	return check, root
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

func checkConfig(root string) doctorCheck {
	check := doctorCheck{Name: "Config"}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg, err := config.LoadConfig(cfgPath)
	if os.IsNotExist(err) {
		check.Status = "warn"
		check.Message = "config.json not found"
		return check
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("invalid: %v", err)
		return check
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("agents: %v", cfg.Agents)
	return check
}

func inspectDoctorMailboxes(root, explicitBaseRoot string, repair, ignoreSessionPins bool) ([]fsq.MailboxInspection, *fsq.MailboxRepairResult, doctorCheck) {
	check := doctorCheck{Name: "Mailboxes"}
	if repair && !ignoreSessionPins {
		mismatch, pinErr := sessionPinMismatch(root)
		if pinErr != nil {
			check.Status = "error"
			check.Message = fmt.Sprintf(
				"refusing to repair %s: invalid AMQ session context: %v; re-run with a complete session context",
				root,
				pinErr,
			)
			return nil, nil, check
		}
		if mismatch != nil && !isPinnedBaseRoot(root) {
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
			if !isSessionRootUnderBase(root, explicitBaseRoot) {
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

func checkSkill(agent string) doctorCheck {
	check := doctorCheck{Name: fmt.Sprintf("%s skill", agent)}

	if agent != "claude" && agent != "codex" {
		check.Status = "warn"
		check.Message = "unknown agent"
		return check
	}

	home, _ := os.UserHomeDir()
	skillDir := filepath.Join(home, "."+agent, "skills", "amq-cli")
	localSkillDir := filepath.Join("."+agent, "skills", "amq-cli")

	// Check project-local skills first, then user-level
	switch {
	case fileExists(filepath.Join(localSkillDir, "SKILL.md")):
		check.Status = "ok"
		check.Message = "installed (project-local)"

	case fileExists(filepath.Join(skillDir, "SKILL.md")):
		check.Status = "ok"
		check.Message = "installed"

	case dirExists(skillDir):
		check.Status = "warn"
		check.Message = "skill directory exists but SKILL.md missing"

	default:
		check.Status = "warn"
		check.Message = "not installed (run: npx skills add avivsinai/agent-message-queue -g -y)"
	}

	return check
}
