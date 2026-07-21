package cli

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

type doctorOpsResult struct {
	Root         opsRoot          `json:"root"`
	Agents       []opsAgent       `json:"agents"`
	OperatorGate *opsOperatorGate `json:"operator_gate,omitempty"`
	WakeLocks    []opsWakeLock    `json:"wake_locks,omitempty"`
	Hints        []opsHint        `json:"hints"`
}

type opsRoot struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

type opsAgent struct {
	Handle                 string  `json:"handle"`
	UnreadCount            int     `json:"unread_count"`
	OldestUnreadAgeSeconds float64 `json:"oldest_unread_age_seconds"`
	DLQCount               int     `json:"dlq_count"`
	OldestDLQAgeSeconds    float64 `json:"oldest_dlq_age_seconds"`
	PresenceStatus         string  `json:"presence_status"`
	PresenceAgeSeconds     float64 `json:"presence_age_seconds"`
	PresenceSource         string  `json:"presence_source,omitempty"`
}

type opsOperatorGate struct {
	OpenCount            int     `json:"open_count"`
	OldestGateAgeSeconds float64 `json:"oldest_gate_age_seconds"`
}

type opsHint struct {
	Code    string `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type opsWakeLock struct {
	Status          string `json:"status"`
	Agent           string `json:"agent"`
	Root            string `json:"root"`
	Lock            string `json:"lock"`
	PID             int    `json:"pid,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Fix             string `json:"fix,omitempty"`
	Removed         bool   `json:"removed,omitempty"`
	Target          string `json:"target,omitempty"`
	TargetPresent   bool   `json:"target_present,omitempty"`
	TargetReason    string `json:"target_reason,omitempty"`
	Repair          string `json:"repair,omitempty"`
	RepairAvailable bool   `json:"repair_available,omitempty"`
}

const fixWakeLocksCommand = "amq doctor --ops --fix-wake-locks"

func runOpsChecks(root string, rootSource string, fixWakeLocks bool) *doctorOpsResult {
	result := &doctorOpsResult{}
	now := time.Now()

	result.Root = opsRoot{
		Path:   root,
		Source: rootSource,
	}
	result.OperatorGate = checkOperatorGate(root, now)
	result.Hints = append(result.Hints, checkLinkedWorktreeLocalHint(root, rootSource)...)

	// Load the active root's config, falling back to the base config for normal
	// session layouts where coop init owns the single config.json.
	agents, err := loadOpsAgents(root, fixWakeLocks)
	if err != nil {
		result.Hints = append(result.Hints, opsHint{
			Code:    "config_error",
			Status:  "error",
			Message: fmt.Sprintf("Cannot load config: %v", err),
		})
		result.WakeLocks = checkWakeLocks(root, discoveredWakeLockAgents(root, nil), fixWakeLocks)
		return result
	}

	validatedAgents := make([]string, 0, len(agents))
	for _, handle := range agents {
		// Configured handles are untrusted input. Validate before deriving any
		// mailbox/presence paths so traversal-like values cannot escape root.
		if err := fsq.ValidateHandle(handle); err != nil {
			result.Hints = append(result.Hints, opsHint{
				Code:    "config_error",
				Status:  "error",
				Message: fmt.Sprintf("Ignoring invalid configured agent handle %q: %v", handle, err),
			})
			continue
		}
		if err := validateWakeLockAgent(root, handle); err != nil {
			result.Hints = append(result.Hints, opsHint{Code: "config_error", Status: "error", Message: fmt.Sprintf("Ignoring unsafe configured agent handle %q: %v", handle, err)})
			continue
		}
		validatedAgents = append(validatedAgents, handle)
		agent := opsAgent{Handle: handle}

		// Unread count + oldest
		inboxNew := fsq.AgentInboxNew(root, handle)
		entries, err := os.ReadDir(inboxNew)
		if err == nil {
			agent.UnreadCount = len(entries)
			for _, e := range entries {
				info, err := e.Info()
				if err == nil {
					age := now.Sub(info.ModTime()).Seconds()
					if age > agent.OldestUnreadAgeSeconds {
						agent.OldestUnreadAgeSeconds = age
					}
				}
			}
		}

		// DLQ count + oldest
		dlqNew := fsq.AgentDLQNew(root, handle)
		dlqEntries, err := os.ReadDir(dlqNew)
		if err == nil {
			agent.DLQCount = len(dlqEntries)
			for _, e := range dlqEntries {
				info, err := e.Info()
				if err == nil {
					age := now.Sub(info.ModTime()).Seconds()
					if age > agent.OldestDLQAgeSeconds {
						agent.OldestDLQAgeSeconds = age
					}
				}
			}
		}

		// Presence
		recentActivity := false
		p, err := presence.Read(root, handle)
		if err == nil {
			agent.PresenceStatus = p.Status
			if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
				agent.PresenceAgeSeconds = now.Sub(t).Seconds()
				recentActivity = agent.PresenceAgeSeconds < (10 * time.Minute).Seconds()
			}
		} else {
			agent.PresenceStatus = "unknown"
		}
		agent.PresenceSource = resolvePresenceSource(root, handle, recentActivity)

		// Round to reasonable precision
		agent.OldestUnreadAgeSeconds = math.Round(agent.OldestUnreadAgeSeconds)
		agent.OldestDLQAgeSeconds = math.Round(agent.OldestDLQAgeSeconds)
		agent.PresenceAgeSeconds = math.Round(agent.PresenceAgeSeconds)

		result.Agents = append(result.Agents, agent)
	}

	// Only handles which passed validation while traversing the config are
	// eligible for diagnostic wake-lock inspection.  Discovery performs the
	// same check for handles found on disk; checkWakeLocks repeats it at its
	// boundary as a defense against future callers passing untrusted input.
	result.WakeLocks = checkWakeLocks(root, discoveredWakeLockAgents(root, validatedAgents), fixWakeLocks)

	// Operational and integration hints
	result.Hints = append(result.Hints, checkSiblingBacklogHints(root, agents)...)
	result.Hints = append(result.Hints, checkWorktreeDivergenceHints(root, agents)...)
	result.Hints = append(result.Hints, checkGlobalRootHint()...)
	result.Hints = append(result.Hints, checkKanbanHint()...)
	result.Hints = append(result.Hints, checkSymphonyHint()...)

	return result
}

func loadOpsAgents(root string, fixWakeLocks bool) ([]string, error) {
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err == nil {
		return cfg.Agents, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	if fixWakeLocks {
		return nil, err
	}
	base := baseRootOfForDisplay(root)
	if absPath(resolveRoot(base)) == absPath(resolveRoot(root)) {
		return nil, err
	}
	baseCfg, baseErr := config.LoadConfig(filepath.Join(base, "meta", "config.json"))
	if baseErr != nil {
		return nil, baseErr
	}
	return baseCfg.Agents, nil
}

func checkSiblingBacklogHints(root string, agents []string) []opsHint {
	current := siblingContext(root)
	var hints []opsHint
	for _, agent := range agents {
		for _, backlog := range findSiblingBacklogs(root, agent) {
			hints = append(hints, opsHint{
				Code:    "sibling_backlog",
				Status:  "warn",
				Message: formatSiblingBacklogHint(backlog, agent, current),
			})
		}
	}
	return hints
}

func checkOperatorGate(root string, now time.Time) *opsOperatorGate {
	inboxNew := fsq.AgentInboxNew(root, reservedHumanHandle)
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		return nil
	}
	gate := &opsOperatorGate{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		gate.OpenCount++
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime()).Seconds()
		if age > gate.OldestGateAgeSeconds {
			gate.OldestGateAgeSeconds = age
		}
	}
	gate.OldestGateAgeSeconds = math.Round(gate.OldestGateAgeSeconds)
	return gate
}

func discoveredWakeLockAgents(root string, configured []string) []string {
	seen := make(map[string]struct{}, len(configured))
	agents := make([]string, 0, len(configured))
	for _, agent := range configured {
		if validateWakeLockAgent(root, agent) != nil {
			continue
		}
		if _, ok := seen[agent]; ok {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}

	discovered, err := fsq.ListAgents(root)
	if err != nil {
		return agents
	}
	for _, agent := range discovered {
		if _, ok := seen[agent]; ok {
			continue
		}
		if err := validateWakeLockAgent(root, agent); err != nil {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}
	return agents
}

func checkWakeLocks(root string, agents []string, fix bool) []opsWakeLock {
	var locks []opsWakeLock
	for _, agent := range agents {
		if validateWakeLockAgent(root, agent) != nil {
			continue
		}
		inspection := inspectWakeLock(root, agent)
		if !inspection.Exists {
			continue
		}

		lock := opsWakeLock{
			Status: string(inspection.Status),
			Agent:  agent,
			Root:   inspection.Root,
			Lock:   inspection.LockPath,
			PID:    inspection.PID,
			Reason: inspection.Reason,
		}
		if isLiveRawOrphan(inspection) {
			lock.Status = "live-raw-orphan"
			lock.Reason = "live raw wake orphan; stop the owning terminal or launchd supervisor"
		}
		target, exists, targetErr := readWakeTarget(root, agent)
		if exists {
			lock.Target = wakeTargetPath(root, agent)
			lock.TargetPresent = true
			if targetErr != nil {
				lock.TargetReason = targetErr.Error()
			} else if err := validateWakeTarget(target, root, agent); err != nil {
				lock.TargetReason = err.Error()
			} else if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
				lock.TargetReason = err.Error()
			}
		}
		if inspection.Status == wakeLockStale {
			lock.Fix = fixWakeLocksCommand
			if lock.TargetPresent && lock.TargetReason == "" && validateWakeLockRepairable(inspection) == nil {
				lock.RepairAvailable = true
				lock.Repair = wakeRepairCommand(root, agent)
			}
			if fix {
				guardErr := withWakeLifecycleGuard(root, agent, func() error {
					recheck := inspectWakeLock(root, agent)
					if !sameWakeLockGeneration(inspection, recheck) || recheck.Status != wakeLockStale {
						lock.Status = string(recheck.Status)
						lock.Reason = "wake lock changed before fix"
						return nil
					}
					if err := validateWakeLockStaleRemoval(recheck); err != nil {
						return err
					}
					if err := removeWakeLockIfUnchangedGuarded(recheck); err != nil {
						return err
					}
					lock.Status = "fixed"
					lock.Removed = true
					return nil
				})
				lock.RepairAvailable = false
				lock.Repair = ""
				if guardErr != nil {
					lock.Status = "error"
					lock.Reason = guardErr.Error()
				}
			}
		}
		locks = append(locks, lock)
	}
	return locks
}

// validateWakeLockAgent ensures diagnostics cannot follow an agent directory
// symlink (including one pointing outside this AMQ root). Missing directories
// remain valid for configured agents: inspectWakeLock will simply report no
// lock, preserving diagnostics for partially initialized roots.
func validateWakeLockAgent(root, agent string) error {
	if err := fsq.ValidateHandle(agent); err != nil {
		return err
	}
	path := fsq.AgentBase(root, agent)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("agent directory is a symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("agent path is not a directory")
	}
	return nil
}

func wakeRepairCommand(root, agent string) string {
	return fmt.Sprintf("amq wake repair --root %s --me %s", shellQuoteArg(root), shellQuoteArg(agent))
}

func shellQuoteArg(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case strings.ContainsRune("@%_+=:,./-", r):
			return false
		default:
			return true
		}
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func checkGlobalRootHint() []opsHint {
	_, err := loadGlobalAmqrc()
	if err != nil {
		globalEnv := os.Getenv(envGlobalRoot)
		if globalEnv == "" {
			return []opsHint{{
				Code:    "global_root_missing",
				Status:  "warn",
				Message: "No global AMQ config found (~/.amqrc or AMQ_GLOBAL_ROOT). Agents spawned by external orchestrators may not find AMQ.",
			}}
		}
		return []opsHint{{
			Code:    "global_root_configured",
			Status:  "ok",
			Message: fmt.Sprintf("Global root configured via AMQ_GLOBAL_ROOT=%s", globalEnv),
		}}
	}
	return []opsHint{{
		Code:    "global_root_configured",
		Status:  "ok",
		Message: "Global root configured via ~/.amqrc",
	}}
}

func checkKanbanHint() []opsHint {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3484", 500*time.Millisecond)
	if err != nil {
		return nil // Kanban not running, no hint needed
	}
	_ = conn.Close()
	return []opsHint{{
		Code:    "kanban_detected",
		Status:  "warn",
		Message: "Cline Kanban appears to be running on 127.0.0.1:3484. Use the experimental 'amq integration kanban bridge' adapter to connect.",
	}}
}

func checkSymphonyHint() []opsHint {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	workflowPath := filepath.Join(cwd, "WORKFLOW.md")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil // No WORKFLOW.md
	}
	content := string(data)
	if strings.Contains(content, "BEGIN AMQ MANAGED") {
		return []opsHint{{
			Code:    "symphony_hooks_installed",
			Status:  "ok",
			Message: "WORKFLOW.md has AMQ integration hooks installed",
		}}
	}
	return []opsHint{{
		Code:    "symphony_workflow_detected",
		Status:  "warn",
		Message: "WORKFLOW.md found but no AMQ hooks. Use 'amq integration symphony init' to install.",
	}}
}
