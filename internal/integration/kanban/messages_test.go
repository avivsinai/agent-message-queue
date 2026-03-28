package kanban

import (
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestBuildNotificationContextIncludesKanbanFields(t *testing.T) {
	note := bridgeNotification{
		Event:         kanbanEventTaskReadyForReview,
		WorkspaceID:   "workspace-1",
		WorkspacePath: "/repo/path",
		TaskID:        "task-1",
		Prompt:        "Refactor bridge",
		Column:        "review",
		State:         sessionStateAwaitingReview,
		ReviewReason:  "hook",
		AgentID:       "codex",
	}

	ctx := buildNotificationContext(note)
	orchestrator, ok := ctx["orchestrator"].(map[string]interface{})
	if !ok {
		t.Fatalf("orchestrator context missing or wrong type: %#v", ctx)
	}
	if orchestrator["name"] != "kanban" {
		t.Fatalf("name = %v, want kanban", orchestrator["name"])
	}

	workspace, ok := orchestrator["workspace"].(map[string]interface{})
	if !ok {
		t.Fatalf("workspace missing or wrong type: %#v", orchestrator["workspace"])
	}
	if workspace["id"] != "workspace-1" {
		t.Fatalf("workspace.id = %v, want workspace-1", workspace["id"])
	}
	if workspace["path"] != "/repo/path" {
		t.Fatalf("workspace.path = %v, want /repo/path", workspace["path"])
	}

	task, ok := orchestrator["task"].(map[string]interface{})
	if !ok {
		t.Fatalf("task missing or wrong type: %#v", orchestrator["task"])
	}
	if task["prompt"] != "Refactor bridge" {
		t.Fatalf("task.prompt = %v, want Refactor bridge", task["prompt"])
	}
	if task["column"] != "review" {
		t.Fatalf("task.column = %v, want review", task["column"])
	}
	if task["review_reason"] != "hook" {
		t.Fatalf("task.review_reason = %v, want hook", task["review_reason"])
	}
}

func TestDeliverNotificationWritesExpectedMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	note := handoffNotification(
		kanbanEventTaskReadyForReview,
		"workspace-1",
		taskSessionSummary{
			TaskID:        "task-1",
			State:         sessionStateAwaitingReview,
			WorkspacePath: "/repo/path",
			ReviewReason:  "hook",
			AgentID:       "codex",
		},
		cardMeta{
			TaskID:        "task-1",
			Prompt:        "Refactor bridge",
			Column:        "review",
			WorkspaceID:   "workspace-1",
			WorkspacePath: "/repo/path",
		},
	)

	msgPath, err := deliverNotification(root, "codex", note)
	if err != nil {
		t.Fatalf("deliverNotification: %v", err)
	}

	msg, err := format.ReadMessageFile(msgPath)
	if err != nil {
		t.Fatalf("ReadMessageFile: %v", err)
	}
	if msg.Header.Thread != "task/task-1" {
		t.Fatalf("Thread = %q, want %q", msg.Header.Thread, "task/task-1")
	}
	if msg.Header.Kind != format.KindTodo {
		t.Fatalf("Kind = %q, want %q", msg.Header.Kind, format.KindTodo)
	}
	if msg.Header.Priority != format.PriorityNormal {
		t.Fatalf("Priority = %q, want %q", msg.Header.Priority, format.PriorityNormal)
	}
	if msg.Header.Subject != "[kanban] review: Refactor bridge" {
		t.Fatalf("Subject = %q", msg.Header.Subject)
	}
	if len(msg.Header.Labels) == 0 {
		t.Fatal("labels should not be empty")
	}
	if filepath.Base(msgPath) == "" {
		t.Fatal("expected non-empty message filename")
	}
}
