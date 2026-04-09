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
	Root   opsRoot    `json:"root"`
	Agents []opsAgent `json:"agents"`
	Hints  []opsHint  `json:"hints"`
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

func runOpsChecks(root string, rootSource string) *doctorOpsResult {
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

	// Integration hints
	result.Hints = append(result.Hints, checkGlobalRootHint()...)
	result.Hints = append(result.Hints, checkKanbanHint()...)
	result.Hints = append(result.Hints, checkSymphonyHint()...)

	return result
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
