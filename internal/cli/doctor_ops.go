package cli

import (
	"context"
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
	Root      opsRoot       `json:"root"`
	Agents    []opsAgent    `json:"agents"`
	WakeLocks []opsWakeLock `json:"wake_locks,omitempty"`
	Hints     []opsHint     `json:"hints"`
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
}

type opsHint struct {
	Code    string `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type opsWakeLock struct {
	Status        string `json:"status"`
	Agent         string `json:"agent"`
	Root          string `json:"root"`
	Lock          string `json:"lock"`
	PID           int    `json:"pid,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Fix           string `json:"fix,omitempty"`
	Removed       bool   `json:"removed,omitempty"`
	Target        string `json:"target,omitempty"`
	TargetPresent bool   `json:"target_present,omitempty"`
	TargetStatus  string `json:"target_status,omitempty"`
	TargetReason  string `json:"target_reason,omitempty"`
	TargetRemoved bool   `json:"target_removed,omitempty"`
}

const fixWakeLocksCommand = "amq doctor --ops --fix-wake-locks"
const fixWakeTargetsCommand = "amq doctor --ops --fix-wake-targets"

var probeWakeGhosttyTarget = func(ctx context.Context, terminalID string) error {
	return probeGhosttyTerminalID(ctx, terminalID)
}

func runOpsChecks(root string, rootSource string, fixWakeLocks bool, fixWakeTargets bool) *doctorOpsResult {
	result := &doctorOpsResult{}
	now := time.Now()

	result.Root = opsRoot{
		Path:   root,
		Source: rootSource,
	}

	// Load config for agent list
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		result.Hints = append(result.Hints, opsHint{
			Code:    "config_error",
			Status:  "error",
			Message: fmt.Sprintf("Cannot load config: %v", err),
		})
		result.WakeLocks = checkWakeLocks(root, discoveredWakeLockAgents(root, nil), fixWakeLocks, fixWakeTargets)
		return result
	}

	for _, handle := range cfg.Agents {
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
		p, err := presence.Read(root, handle)
		if err == nil {
			agent.PresenceStatus = p.Status
			if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
				agent.PresenceAgeSeconds = now.Sub(t).Seconds()
			}
		} else {
			agent.PresenceStatus = "unknown"
		}

		// Round to reasonable precision
		agent.OldestUnreadAgeSeconds = math.Round(agent.OldestUnreadAgeSeconds)
		agent.OldestDLQAgeSeconds = math.Round(agent.OldestDLQAgeSeconds)
		agent.PresenceAgeSeconds = math.Round(agent.PresenceAgeSeconds)

		result.Agents = append(result.Agents, agent)
	}

	result.WakeLocks = checkWakeLocks(root, discoveredWakeLockAgents(root, cfg.Agents), fixWakeLocks, fixWakeTargets)

	// Integration hints
	result.Hints = append(result.Hints, checkGlobalRootHint()...)
	result.Hints = append(result.Hints, checkKanbanHint()...)
	result.Hints = append(result.Hints, checkSymphonyHint()...)

	return result
}

func discoveredWakeLockAgents(root string, configured []string) []string {
	seen := make(map[string]struct{}, len(configured))
	agents := make([]string, 0, len(configured))
	for _, agent := range configured {
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
		if err := fsq.ValidateHandle(agent); err != nil {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}
	return agents
}

func checkWakeLocks(root string, agents []string, fixLocks bool, fixTargets bool) []opsWakeLock {
	var locks []opsWakeLock
	for _, agent := range agents {
		inspection := inspectWakeLock(root, agent)
		if !inspection.Exists {
			if targetLock, ok := inspectOrphanedWakeTarget(root, agent, fixTargets); ok {
				locks = append(locks, targetLock)
			}
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
		applyWakeTargetHealth(root, agent, inspection, &lock)
		if inspection.Status == wakeLockStale {
			lock.Fix = fixWakeLocksCommand
			if fixLocks {
				recheck := inspectWakeLock(root, agent)
				if recheck.Status != wakeLockStale {
					lock.Status = string(recheck.Status)
					lock.Reason = "wake lock changed before fix"
				} else if err := removeWakeLockIfUnchanged(recheck); err != nil {
					lock.Status = "error"
					lock.Reason = err.Error()
				} else {
					lock.Status = "fixed"
					lock.Removed = true
					if fixTargets && lock.TargetPresent && lock.TargetStatus == "bound" {
						if err := removeWakeTarget(root, agent); err != nil {
							lock.Status = "error"
							lock.Reason = err.Error()
						} else {
							lock.TargetRemoved = true
							lock.TargetStatus = "fixed"
						}
					}
				}
			}
		}
		locks = append(locks, lock)
	}
	return locks
}

func inspectOrphanedWakeTarget(root, agent string, fix bool) (opsWakeLock, bool) {
	lock := opsWakeLock{
		Status: string(wakeLockMissing),
		Agent:  agent,
		Root:   canonicalWakeRoot(root),
		Lock:   filepath.Join(fsq.AgentBase(root, agent), ".wake.lock"),
	}
	target, exists, targetErr := readWakeTarget(root, agent)
	if !exists {
		return opsWakeLock{}, false
	}
	lock.Target = wakeTargetPath(root, agent)
	lock.TargetPresent = true
	if targetErr != nil {
		lock.TargetStatus = "invalid"
		lock.TargetReason = targetErr.Error()
	} else if err := validateWakeTarget(target, root, agent); err != nil {
		lock.TargetStatus = "invalid"
		lock.TargetReason = err.Error()
	} else {
		lock.TargetStatus = "orphaned"
		lock.TargetReason = "wake target has no wake lock"
	}
	lock.Fix = fixWakeTargetsCommand
	if fix {
		if err := removeWakeTarget(root, agent); err != nil {
			lock.Status = "error"
			lock.TargetReason = err.Error()
		} else {
			lock.Status = "fixed"
			lock.TargetRemoved = true
			lock.TargetStatus = "fixed"
			lock.TargetReason = ""
		}
	}
	return lock, true
}

func applyWakeTargetHealth(root, agent string, inspection wakeLockInspection, lock *opsWakeLock) {
	target, exists, targetErr := readWakeTarget(root, agent)
	if !exists {
		return
	}
	lock.Target = wakeTargetPath(root, agent)
	lock.TargetPresent = true
	if targetErr != nil {
		lock.TargetStatus = "invalid"
		lock.TargetReason = targetErr.Error()
		return
	}
	if err := validateWakeTarget(target, root, agent); err != nil {
		lock.TargetStatus = "invalid"
		lock.TargetReason = err.Error()
		return
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
		lock.TargetStatus = "mismatched"
		lock.TargetReason = err.Error()
		return
	}
	if status, reason := ghosttyWakeTargetStatus(target); status != "" {
		lock.TargetStatus = status
		lock.TargetReason = reason
		return
	}
	lock.TargetStatus = "bound"
}

func ghosttyWakeTargetStatus(target wakeTarget) (status, reason string) {
	if target.Mode != wakeTargetGhostty {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultInjectTimeout)
	defer cancel()
	if err := probeWakeGhosttyTarget(ctx, target.GhosttyTerminalID); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "no Ghostty terminal with id"):
			return "missing-terminal", msg
		case strings.Contains(msg, "ambiguous Ghostty terminal id"):
			return "ambiguous-terminal", msg
		default:
			return "invalid", msg
		}
	}
	return "", ""
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
