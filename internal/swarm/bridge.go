package swarm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

// BridgeConfig holds configuration for the swarm bridge process.
type BridgeConfig struct {
	TeamName     string
	AgentHandle  string        // AMQ handle for the external agent (e.g., "codex")
	AgentID      string        // Agent Teams agent_id
	AMQRoot      string        // AMQ root directory
	PollInterval time.Duration // How often to check for task changes (polling mode)
	UsePoll      bool          // Force polling instead of fsnotify
}

// BridgeEvent represents a task change detected by the bridge.
type BridgeEvent struct {
	Type    string `json:"type"` // "task_added", "task_updated", "task_assigned", "task_unblocked", "task_completed"
	TaskID  string `json:"task_id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
}

// BridgeMode returns "fsnotify" or "polling" based on cfg.UsePoll.
func BridgeMode(cfg BridgeConfig) string {
	if cfg.UsePoll {
		return "polling"
	}

	// Detect whether fsnotify is actually usable for this team on this host.
	// This matches RunBridge's fallback behavior (fsnotify -> polling).
	tasksDir := TeamTasksDir(cfg.TeamName)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return "polling"
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(tasksDir); err != nil {
		return "polling"
	}

	return "fsnotify"
}

// RunBridge starts the bridge process that syncs between Agent Teams tasks
// and AMQ messages. Uses fsnotify by default, falling back to polling on error
// or when cfg.UsePoll is set.
func RunBridge(ctx context.Context, cfg BridgeConfig) error {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}

	// Ensure agent inbox exists once at startup, not per-message.
	if err := fsq.EnsureAgentDirs(cfg.AMQRoot, cfg.AgentHandle); err != nil {
		return fmt.Errorf("ensure agent dirs: %w", err)
	}

	lastStates, lastDepsSatisfied, err := initialScan(cfg)
	if err != nil {
		return err
	}

	if cfg.UsePoll {
		return bridgeWithPolling(ctx, cfg, lastStates, lastDepsSatisfied)
	}
	return bridgeWithFsnotify(ctx, cfg, lastStates, lastDepsSatisfied)
}

func initialScan(cfg BridgeConfig) (map[string]string, map[string]bool, error) {
	lastStates := make(map[string]string)
	lastDepsSatisfied := make(map[string]bool)

	tasks, err := ListTasks(cfg.TeamName)
	if err != nil {
		return nil, nil, fmt.Errorf("initial task scan: %w", err)
	}
	statusMap := make(map[string]string, len(tasks))
	for _, t := range tasks {
		lastStates[t.ID] = t.Status + ":" + t.AssignedTo
		statusMap[t.ID] = t.Status
	}
	for _, t := range tasks {
		lastDepsSatisfied[t.ID] = allDepsCompleted(t.DependsOn, statusMap)
	}
	return lastStates, lastDepsSatisfied, nil
}

func bridgeWithPolling(ctx context.Context, cfg BridgeConfig, lastStates map[string]string, lastDepsSatisfied map[string]bool) error {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			processPollEvents(cfg, lastStates, lastDepsSatisfied)
		}
	}
}

func bridgeWithFsnotify(ctx context.Context, cfg BridgeConfig, lastStates map[string]string, lastDepsSatisfied map[string]bool) error {
	tasksDir := TeamTasksDir(cfg.TeamName)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bridge: fsnotify unavailable (%v), falling back to polling\n", err)
		return bridgeWithPolling(ctx, cfg, lastStates, lastDepsSatisfied)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(tasksDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bridge: cannot watch %s (%v), falling back to polling\n", tasksDir, err)
		return bridgeWithPolling(ctx, cfg, lastStates, lastDepsSatisfied)
	}

	// Catch-up poll: events between initialScan and watcher.Add would be missed.
	processPollEvents(cfg, lastStates, lastDepsSatisfied)

	// Debounce: coalesce rapid filesystem events into a single poll.
	// 50ms is longer than watch.go's 10ms because task file writes may
	// involve multiple steps (write tmp + rename).
	const debounceInterval = 50 * time.Millisecond
	trigger := make(chan struct{}, 1)

	var mu sync.Mutex
	var debounceTimer *time.Timer

	resetDebounce := func() {
		mu.Lock()
		defer mu.Unlock()
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(debounceInterval, func() {
			select {
			case trigger <- struct{}{}:
			default:
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			mu.Unlock()
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("bridge: watcher closed")
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				resetDebounce()
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("bridge: watcher closed")
			}
			_, _ = fmt.Fprintf(os.Stderr, "bridge: watcher error: %v\n", err)

		case <-trigger:
			processPollEvents(cfg, lastStates, lastDepsSatisfied)
		}
	}
}

func processPollEvents(cfg BridgeConfig, lastStates map[string]string, lastDepsSatisfied map[string]bool) {
	events, err := pollForChanges(cfg, lastStates, lastDepsSatisfied)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bridge: poll error: %v\n", err)
		return
	}
	for _, event := range events {
		if err := deliverBridgeEvent(cfg, event); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "bridge: delivery error: %v\n", err)
		}
	}
}

func pollForChanges(cfg BridgeConfig, lastStates map[string]string, lastDepsSatisfied map[string]bool) ([]BridgeEvent, error) {
	tasks, err := ListTasks(cfg.TeamName)
	if err != nil {
		return nil, err
	}

	// Build status map for dependency checking
	statusMap := make(map[string]string, len(tasks))
	for _, t := range tasks {
		statusMap[t.ID] = t.Status
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
				if task.Status == TaskStatusInProgress && isAssignedToMe(task.AssignedTo, cfg.AgentHandle, cfg.AgentID) {
					eventType = "task_assigned"
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

		// Check for dependency unblocking (separate from state changes).
		// Only emit for previously-known tasks to avoid dual task_added+task_unblocked.
		if known && len(task.DependsOn) > 0 && task.Status == TaskStatusPending {
			depsSatisfied := allDepsCompleted(task.DependsOn, statusMap)
			wasSatisfied := lastDepsSatisfied[task.ID]
			if depsSatisfied && !wasSatisfied {
				if isRelevantToAgent(task, cfg.AgentHandle, cfg.AgentID) {
					events = append(events, BridgeEvent{
						Type:    "task_unblocked",
						TaskID:  task.ID,
						Title:   task.Title,
						Status:  task.Status,
						Details: fmt.Sprintf("dependencies satisfied: %v", task.DependsOn),
					})
				}
			}
			lastDepsSatisfied[task.ID] = depsSatisfied
		} else if len(task.DependsOn) > 0 && task.Status == TaskStatusPending {
			// New task with deps — record current state without emitting
			lastDepsSatisfied[task.ID] = allDepsCompleted(task.DependsOn, statusMap)
		} else {
			// No deps or not pending — mark as satisfied (vacuously true)
			lastDepsSatisfied[task.ID] = true
		}

		lastStates[task.ID] = state
	}

	// Detect removed tasks
	for id := range lastStates {
		if !currentIDs[id] {
			delete(lastStates, id)
			delete(lastDepsSatisfied, id)
		}
	}

	return events, nil
}

// allDepsCompleted returns true if all dependency task IDs have status "completed".
func allDepsCompleted(deps []string, statusMap map[string]string) bool {
	for _, dep := range deps {
		if statusMap[dep] != TaskStatusCompleted {
			return false
		}
	}
	return true
}

func isRelevantToAgent(task Task, handle, agentID string) bool {
	// Task is relevant if assigned to this agent. Unassigned tasks are also
	// considered relevant so bridges can surface claimable work.
	//
	// Note: If multiple external agents run bridges for the same team, unassigned
	// tasks will fan out to all of them.
	if task.AssignedTo == "" {
		return true
	}
	return isAssignedToMe(task.AssignedTo, handle, agentID)
}

func isAssignedToMe(assignedTo, handle, agentID string) bool {
	return strings.EqualFold(assignedTo, handle) || strings.EqualFold(assignedTo, agentID)
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
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     "swarm-bridge",
			To:       []string{cfg.AgentHandle},
			Thread:   fmt.Sprintf("swarm/%s", cfg.TeamName),
			Subject:  subject,
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Kind:     format.KindStatus,
			Labels:   []string{"swarm", event.Type},
			Context: map[string]any{
				"team":    cfg.TeamName,
				"task_id": event.TaskID,
				"event":   event.Type,
				"source":  "swarm-bridge",
			},
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	_, err = fsq.DeliverToInboxes(cfg.AMQRoot, msg.Header.To, filename, data)
	return err
}
