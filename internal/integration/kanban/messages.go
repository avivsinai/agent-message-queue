package kanban

import (
	"fmt"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/integration/common"
)

type bridgeNotification struct {
	Event         string
	WorkspaceID   string
	WorkspacePath string
	TaskID        string
	Prompt        string
	Column        string
	State         string
	ReviewReason  string
	AgentID       string
	Kind          string
	Priority      string
	ExtraLabels   []string
}

func deliverNotification(root, me string, note bridgeNotification) (string, error) {
	if note.TaskID == "" {
		return "", fmt.Errorf("task id is required")
	}
	return common.DeliverIntegrationMessage(
		root,
		me,
		me,
		buildNotificationSubject(note),
		buildNotificationBody(note),
		buildNotificationContext(note),
		buildNotificationLabels(note),
		"task/"+note.TaskID,
		note.Kind,
		note.Priority,
	)
}

func buildNotificationContext(note bridgeNotification) map[string]interface{} {
	workspace := map[string]interface{}{}
	if note.WorkspaceID != "" {
		workspace["id"] = note.WorkspaceID
	}
	if note.WorkspacePath != "" {
		workspace["path"] = note.WorkspacePath
	}

	task := map[string]interface{}{
		"id": note.TaskID,
	}
	if note.Prompt != "" {
		task["prompt"] = note.Prompt
	}
	if note.Column != "" {
		task["column"] = note.Column
	}
	if note.State != "" {
		task["state"] = note.State
	}
	if note.ReviewReason != "" {
		task["review_reason"] = note.ReviewReason
	}
	if note.AgentID != "" {
		task["agent_id"] = note.AgentID
	}

	orchestrator := map[string]interface{}{
		"version":   1,
		"name":      "kanban",
		"transport": "bridge",
		"event":     note.Event,
	}
	if len(workspace) > 0 {
		orchestrator["workspace"] = workspace
	}
	if len(task) > 0 {
		orchestrator["task"] = task
	}

	return map[string]interface{}{
		"orchestrator": orchestrator,
	}
}

func buildNotificationLabels(note bridgeNotification) []string {
	return common.BuildOrchestratorLabels("kanban", note.State, note.ExtraLabels...)
}

func buildNotificationSubject(note bridgeNotification) string {
	display := note.displayTitle()
	switch note.State {
	case sessionStateAwaitingReview:
		return fmt.Sprintf("[kanban] review: %s", display)
	case sessionStateRunning:
		return fmt.Sprintf("[kanban] running: %s", display)
	case sessionStateFailed:
		return fmt.Sprintf("[kanban] failed: %s", display)
	case sessionStateInterrupted:
		return fmt.Sprintf("[kanban] interrupted: %s", display)
	default:
		return fmt.Sprintf("[kanban] %s: %s", note.Event, display)
	}
}

func buildNotificationBody(note bridgeNotification) string {
	lines := []string{
		fmt.Sprintf("Event: %s", note.Event),
		fmt.Sprintf("Task: %s", note.TaskID),
	}
	if note.Prompt != "" {
		lines = append(lines, fmt.Sprintf("Prompt: %s", note.Prompt))
	}
	if note.State != "" {
		lines = append(lines, fmt.Sprintf("State: %s", note.State))
	}
	if note.Column != "" {
		lines = append(lines, fmt.Sprintf("Column: %s", note.Column))
	}
	if note.ReviewReason != "" {
		lines = append(lines, fmt.Sprintf("Review Reason: %s", note.ReviewReason))
	}
	if note.AgentID != "" {
		lines = append(lines, fmt.Sprintf("Agent: %s", note.AgentID))
	}
	if note.WorkspaceID != "" {
		lines = append(lines, fmt.Sprintf("Workspace ID: %s", note.WorkspaceID))
	}
	if note.WorkspacePath != "" {
		lines = append(lines, fmt.Sprintf("Workspace Path: %s", note.WorkspacePath))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (n bridgeNotification) displayTitle() string {
	if strings.TrimSpace(n.Prompt) != "" {
		return n.Prompt
	}
	return n.TaskID
}

func runningNotification(workspaceID string, summary taskSessionSummary, meta cardMeta) bridgeNotification {
	return bridgeNotification{
		Event:         kanbanEventTaskSessionsUpdated,
		WorkspaceID:   workspaceID,
		WorkspacePath: firstNonEmpty(summary.WorkspacePath, meta.WorkspacePath),
		TaskID:        summary.TaskID,
		Prompt:        meta.Prompt,
		Column:        meta.Column,
		State:         summary.State,
		AgentID:       summary.AgentID,
		Kind:          format.KindStatus,
		Priority:      format.PriorityLow,
	}
}

func blockingNotification(workspaceID string, summary taskSessionSummary, meta cardMeta) bridgeNotification {
	return bridgeNotification{
		Event:         kanbanEventTaskSessionsUpdated,
		WorkspaceID:   workspaceID,
		WorkspacePath: firstNonEmpty(summary.WorkspacePath, meta.WorkspacePath),
		TaskID:        summary.TaskID,
		Prompt:        meta.Prompt,
		Column:        meta.Column,
		State:         summary.State,
		AgentID:       summary.AgentID,
		Kind:          format.KindTodo,
		Priority:      format.PriorityNormal,
		ExtraLabels:   []string{"blocking"},
	}
}

func handoffNotification(event, workspaceID string, summary taskSessionSummary, meta cardMeta) bridgeNotification {
	return bridgeNotification{
		Event:         event,
		WorkspaceID:   workspaceID,
		WorkspacePath: firstNonEmpty(summary.WorkspacePath, meta.WorkspacePath),
		TaskID:        summary.TaskID,
		Prompt:        meta.Prompt,
		Column:        meta.Column,
		State:         sessionStateAwaitingReview,
		ReviewReason:  summary.ReviewReason,
		AgentID:       summary.AgentID,
		Kind:          format.KindTodo,
		Priority:      format.PriorityNormal,
		ExtraLabels:   []string{"handoff"},
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
