package swarm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// BridgeConfig holds configuration for the swarm bridge process.
type BridgeConfig struct {
	TeamName     string
	AgentHandle  string        // AMQ handle for the external agent (e.g., "codex")
	AgentID      string        // Agent Teams agent_id
	AMQRoot      string        // AMQ root directory
	PollInterval time.Duration // How often to check for task changes
}

// BridgeEvent represents a task change detected by the bridge.
type BridgeEvent struct {
	Type    string `json:"type"` // "task_assigned", "task_unblocked", "task_completed"
	TaskID  string `json:"task_id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
}

// RunBridge starts the bridge process that syncs between Agent Teams tasks
// and AMQ messages. It polls the task list for changes relevant to the
// external agent and delivers notifications via AMQ.
func RunBridge(ctx context.Context, cfg BridgeConfig) error {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}

	// Track last known task states to detect changes
	lastStates := make(map[string]string) // taskID → "status:assigned_to"

	// Initial scan
	tasks, err := ListTasks(cfg.TeamName)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("initial task scan: %w", err)
	}
	for _, t := range tasks {
		lastStates[t.ID] = t.Status + ":" + t.AssignedTo
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			events, err := pollForChanges(cfg, lastStates)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "bridge: poll error: %v\n", err)
				continue
			}
			for _, event := range events {
				if err := deliverBridgeEvent(cfg, event); err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "bridge: delivery error: %v\n", err)
				}
			}
		}
	}
}

func pollForChanges(cfg BridgeConfig, lastStates map[string]string) ([]BridgeEvent, error) {
	tasks, err := ListTasks(cfg.TeamName)
	if err != nil {
		return nil, err
	}

	var events []BridgeEvent
	currentIDs := make(map[string]bool)

	for _, task := range tasks {
		currentIDs[task.ID] = true
		state := task.Status + ":" + task.AssignedTo
		prev, known := lastStates[task.ID]

		if !known {
			// New task
			if isRelevantToAgent(task, cfg.AgentHandle, cfg.AgentID) {
				events = append(events, BridgeEvent{
					Type:   "task_added",
					TaskID: task.ID,
					Title:  task.Title,
					Status: task.Status,
				})
			}
		} else if state != prev {
			// State changed
			if isRelevantToAgent(task, cfg.AgentHandle, cfg.AgentID) {
				eventType := "task_updated"
				if task.Status == TaskStatusInProgress && strings.Contains(task.AssignedTo, cfg.AgentHandle) {
					eventType = "task_assigned"
				} else if task.Status == TaskStatusPending && !strings.Contains(prev, TaskStatusPending) {
					eventType = "task_unblocked"
				} else if task.Status == TaskStatusCompleted {
					eventType = "task_completed"
				}
				events = append(events, BridgeEvent{
					Type:    eventType,
					TaskID:  task.ID,
					Title:   task.Title,
					Status:  task.Status,
					Details: fmt.Sprintf("assigned_to=%s", task.AssignedTo),
				})
			}
		}

		lastStates[task.ID] = state
	}

	// Detect removed tasks
	for id := range lastStates {
		if !currentIDs[id] {
			delete(lastStates, id)
		}
	}

	return events, nil
}

func isRelevantToAgent(task Task, handle, agentID string) bool {
	// Task is relevant if assigned to this agent or unassigned (claimable)
	if task.AssignedTo == "" {
		return true
	}
	lower := strings.ToLower(task.AssignedTo)
	return lower == handle || lower == agentID || strings.Contains(lower, handle)
}

func deliverBridgeEvent(cfg BridgeConfig, event BridgeEvent) error {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("[swarm] %s: %s", event.Type, event.Title)
	body := fmt.Sprintf("Task: %s\nStatus: %s\nType: %s\n", event.TaskID, event.Status, event.Type)
	if event.Details != "" {
		body += fmt.Sprintf("Details: %s\n", event.Details)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "swarm-bridge",
			To:      []string{cfg.AgentHandle},
			Thread:  fmt.Sprintf("swarm/%s", cfg.TeamName),
			Subject: subject,
			Created: now.UTC().Format(time.RFC3339Nano),
			Kind:    format.KindStatus,
			Labels:  []string{"swarm", event.Type},
			Context: map[string]any{
				"team":    cfg.TeamName,
				"task_id": event.TaskID,
				"event":   event.Type,
			},
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	recipients := []string{cfg.AgentHandle}

	// Ensure agent dirs exist
	if err := fsq.EnsureAgentDirs(cfg.AMQRoot, cfg.AgentHandle); err != nil {
		return err
	}
	// Also ensure swarm-bridge outbox
	outboxDir := filepath.Join(cfg.AMQRoot, "agents", "swarm-bridge", "outbox", "sent")
	if err := os.MkdirAll(outboxDir, 0o700); err != nil {
		return err
	}

	if _, err := fsq.DeliverToInboxes(cfg.AMQRoot, recipients, filename, data); err != nil {
		return err
	}

	// Best-effort outbox copy
	_, _ = fsq.WriteFileAtomic(outboxDir, filename, data, 0o600)

	return nil
}
