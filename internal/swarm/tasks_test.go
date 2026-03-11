package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTasksDir(t *testing.T, teamName string) (home string, tasksDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	tasksDir = filepath.Join(home, claudeConfigDir, tasksSubdir, teamName)
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, tasksDir
}

func writeTasksJSON(t *testing.T, dir string, tasks []map[string]any) {
	t.Helper()
	wrapper := map[string]any{"tasks": tasks}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTaskFile(t *testing.T, dir, filename string, task map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- ListTasks ---

func TestListTasks_SingleFile(t *testing.T) {
	_, dir := setupTasksDir(t, "myteam")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "First", "status": "pending"},
		{"id": "t2", "title": "Second", "status": "in_progress", "assigned_to": "codex"},
	})

	tasks, err := ListTasks("myteam")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len = %d, want 2", len(tasks))
	}
	if tasks[0].ID != "t1" || tasks[1].ID != "t2" {
		t.Errorf("IDs = [%s, %s], want [t1, t2]", tasks[0].ID, tasks[1].ID)
	}
	if tasks[1].AssignedTo != "codex" {
		t.Errorf("tasks[1].AssignedTo = %q, want %q", tasks[1].AssignedTo, "codex")
	}
}

func TestListTasks_PerFile(t *testing.T) {
	_, dir := setupTasksDir(t, "myteam")
	writeTaskFile(t, dir, "task-a.json", map[string]any{"id": "a", "title": "Alpha", "status": "pending"})
	writeTaskFile(t, dir, "task-b.json", map[string]any{"id": "b", "title": "Beta", "status": "completed"})

	tasks, err := ListTasks("myteam")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len = %d, want 2", len(tasks))
	}
}

func TestListTasks_Empty(t *testing.T) {
	setupTasksDir(t, "empty-team")

	tasks, err := ListTasks("empty-team")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("len = %d, want 0", len(tasks))
	}
}

func TestListTasks_NoDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	tasks, err := ListTasks("nonexistent")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if tasks != nil {
		t.Errorf("expected nil, got %v", tasks)
	}
}

// --- ClaimTask ---

func TestClaimTask_SingleFile(t *testing.T) {
	_, dir := setupTasksDir(t, "team1")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Do stuff", "status": "pending"},
	})

	if err := ClaimTask("team1", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	tasks, err := ListTasks("team1")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
	if tasks[0].AssignedTo != "codex" {
		t.Errorf("assigned_to = %q, want %q", tasks[0].AssignedTo, "codex")
	}
}

func TestClaimTask_PerFile(t *testing.T) {
	_, dir := setupTasksDir(t, "team2")
	writeTaskFile(t, dir, "my-task.json", map[string]any{
		"id": "mt1", "title": "Per-file task", "status": "pending",
	})

	if err := ClaimTask("team2", "mt1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	tasks, err := ListTasks("team2")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
}

func TestClaimTask_PerFile_WrapperFormat(t *testing.T) {
	_, dir := setupTasksDir(t, "team-wrapper")
	// Per-file JSON using wrapper format {"tasks": [...]}
	writeTasksJSON(t, dir, nil) // remove tasks.json so per-file is used
	_ = os.Remove(filepath.Join(dir, "tasks.json"))

	// Write a per-file that uses wrapper format
	wrapper := map[string]any{
		"tasks": []any{
			map[string]any{"id": "w1", "title": "Wrapped task", "status": "pending"},
		},
	}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "batch.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ClaimTask("team-wrapper", "w1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	tasks, err := ListTasks("team-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len = %d, want 1", len(tasks))
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
	if tasks[0].AssignedTo != "codex" {
		t.Errorf("assigned_to = %q, want %q", tasks[0].AssignedTo, "codex")
	}
}

func TestClaimTask_AlreadyAssigned(t *testing.T) {
	_, dir := setupTasksDir(t, "team3")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Taken", "status": "in_progress", "assigned_to": "claude"},
	})

	err := ClaimTask("team3", "t1", "codex")
	if err == nil {
		t.Fatal("expected error for already-assigned task")
	}
}

func TestClaimTask_AlreadyCompleted(t *testing.T) {
	_, dir := setupTasksDir(t, "team4")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Done", "status": "completed"},
	})

	err := ClaimTask("team4", "t1", "codex")
	if err == nil {
		t.Fatal("expected error for completed task")
	}
}

func TestClaimTask_NotFound(t *testing.T) {
	_, dir := setupTasksDir(t, "team5")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Exists", "status": "pending"},
	})

	err := ClaimTask("team5", "nonexistent", "codex")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// --- ClaimTask dependency gating ---

func TestClaimTask_BlockedByDependency(t *testing.T) {
	_, dir := setupTasksDir(t, "dep-team1")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Prereq", "status": "pending"},
		{"id": "t2", "title": "Blocked", "status": "pending", "depends_on": []any{"t1"}},
	})

	err := ClaimTask("dep-team1", "t2", "codex")
	if err == nil {
		t.Fatal("expected error for blocked task")
	}
	if !strings.Contains(err.Error(), "blocked by incomplete dependencies") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "t1") {
		t.Errorf("error should mention blocking task t1: %v", err)
	}
}

func TestClaimTask_DependencySatisfied(t *testing.T) {
	_, dir := setupTasksDir(t, "dep-team2")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Prereq", "status": "completed"},
		{"id": "t2", "title": "Ready", "status": "pending", "depends_on": []any{"t1"}},
	})

	if err := ClaimTask("dep-team2", "t2", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	tasks, err := ListTasks("dep-team2")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == "t2" {
			if task.Status != TaskStatusInProgress {
				t.Errorf("status = %q, want %q", task.Status, TaskStatusInProgress)
			}
			return
		}
	}
	t.Fatal("task t2 not found")
}

func TestClaimTask_EmptyDependencies(t *testing.T) {
	_, dir := setupTasksDir(t, "dep-team3")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "No deps", "status": "pending", "depends_on": []any{}},
	})

	if err := ClaimTask("dep-team3", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	tasks, err := ListTasks("dep-team3")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
}

// --- CompleteTask ---

func TestCompleteTask(t *testing.T) {
	_, dir := setupTasksDir(t, "team6")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "WIP", "status": "in_progress", "assigned_to": "codex"},
	})

	if err := CompleteTask("team6", "t1", "codex", "codex", nil); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	tasks, err := ListTasks("team6")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Status != TaskStatusCompleted {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusCompleted)
	}
}

func TestCompleteTask_WrongAgent(t *testing.T) {
	_, dir := setupTasksDir(t, "team7")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "WIP", "status": "in_progress", "assigned_to": "claude"},
	})

	err := CompleteTask("team7", "t1", "codex", "codex", nil)
	if err == nil {
		t.Fatal("expected error for wrong agent")
	}
}

func TestCompleteTask_Pending(t *testing.T) {
	_, dir := setupTasksDir(t, "team-complete-pending")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Not started", "status": "pending", "assigned_to": "codex"},
	})

	err := CompleteTask("team-complete-pending", "t1", "codex", "codex", nil)
	if err == nil {
		t.Fatal("expected error for completing a pending task")
	}
	if !strings.Contains(err.Error(), "not in progress") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompleteTask_AlreadyCompleted(t *testing.T) {
	_, dir := setupTasksDir(t, "team-complete-done")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Done", "status": "completed", "assigned_to": "codex"},
	})

	err := CompleteTask("team-complete-done", "t1", "codex", "codex", nil)
	if err == nil {
		t.Fatal("expected error for completing an already-completed task")
	}
	if !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompleteTask_WithEvidence(t *testing.T) {
	_, dir := setupTasksDir(t, "team-complete-evidence")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "WIP", "status": "in_progress", "assigned_to": "codex"},
	})

	evidence := map[string]any{
		"tests_passed":  true,
		"ci_status":     "green",
		"files_changed": []any{"internal/cli/swarm.go"},
	}

	if err := CompleteTask("team-complete-evidence", "t1", "codex", "codex", evidence); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}

	tasksRaw, ok := wrapper["tasks"].([]any)
	if !ok || len(tasksRaw) != 1 {
		t.Fatalf("unexpected tasks wrapper: %v", wrapper["tasks"])
	}
	task, ok := tasksRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("task is not a map: %T", tasksRaw[0])
	}
	storedEvidence, ok := task["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("evidence not stored as object: %T", task["evidence"])
	}
	if storedEvidence["ci_status"] != "green" {
		t.Errorf("ci_status = %v, want %q", storedEvidence["ci_status"], "green")
	}
	filesChanged, ok := storedEvidence["files_changed"].([]any)
	if !ok || len(filesChanged) != 1 || filesChanged[0] != "internal/cli/swarm.go" {
		t.Errorf("files_changed = %v, want single swarm.go path", storedEvidence["files_changed"])
	}
}

func TestFailTask(t *testing.T) {
	_, dir := setupTasksDir(t, "team-fail")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Broken", "status": "in_progress", "assigned_to": "codex"},
	})

	if err := FailTask("team-fail", "t1", "codex", "ext_codex_1", "tests are red"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	tasksRaw := wrapper["tasks"].([]any)
	task := tasksRaw[0].(map[string]any)
	if task["status"] != TaskStatusFailed {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusFailed)
	}
	if task["failure_reason"] != "tests are red" {
		t.Errorf("failure_reason = %v, want %q", task["failure_reason"], "tests are red")
	}
}

func TestBlockTask(t *testing.T) {
	_, dir := setupTasksDir(t, "team-block")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Waiting", "status": "in_progress", "assigned_to": "codex"},
	})

	if err := BlockTask("team-block", "t1", "codex", "ext_codex_1", "waiting on API access"); err != nil {
		t.Fatalf("BlockTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	tasksRaw := wrapper["tasks"].([]any)
	task := tasksRaw[0].(map[string]any)
	if task["status"] != TaskStatusBlocked {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusBlocked)
	}
	if task["block_reason"] != "waiting on API access" {
		t.Errorf("block_reason = %v, want %q", task["block_reason"], "waiting on API access")
	}
}

func TestFailTask_OnlyAssigneeCanFail(t *testing.T) {
	_, dir := setupTasksDir(t, "team-fail-owner")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Broken", "status": "in_progress", "assigned_to": "claude"},
	})

	err := FailTask("team-fail-owner", "t1", "codex", "ext_codex_1", "not mine")
	if err == nil {
		t.Fatal("expected error for wrong agent")
	}

	tasks, listErr := ListTasks("team-fail-owner")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
}

func TestBlockTask_OnlyAssigneeCanBlock(t *testing.T) {
	_, dir := setupTasksDir(t, "team-block-owner")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Waiting", "status": "in_progress", "assigned_to": "claude"},
	})

	err := BlockTask("team-block-owner", "t1", "codex", "ext_codex_1", "not mine")
	if err == nil {
		t.Fatal("expected error for wrong agent")
	}

	tasks, listErr := ListTasks("team-block-owner")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if tasks[0].Status != TaskStatusInProgress {
		t.Errorf("status = %q, want %q", tasks[0].Status, TaskStatusInProgress)
	}
}

func TestFailTask_RequiresInProgress(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "pending", status: TaskStatusPending},
		{name: "completed", status: TaskStatusCompleted},
		{name: "failed", status: TaskStatusFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, dir := setupTasksDir(t, "team-fail-"+tt.name)
			writeTasksJSON(t, dir, []map[string]any{
				{"id": "t1", "title": "Stateful", "status": tt.status, "assigned_to": "codex"},
			})

			err := FailTask("team-fail-"+tt.name, "t1", "codex", "codex", "broken")
			if err == nil {
				t.Fatalf("expected error for status %q", tt.status)
			}
			want := "not in progress"
			if tt.status == TaskStatusFailed {
				want = "already failed"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBlockTask_RequiresInProgress(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "pending", status: TaskStatusPending},
		{name: "completed", status: TaskStatusCompleted},
		{name: "blocked", status: TaskStatusBlocked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, dir := setupTasksDir(t, "team-block-"+tt.name)
			writeTasksJSON(t, dir, []map[string]any{
				{"id": "t1", "title": "Stateful", "status": tt.status, "assigned_to": "codex"},
			})

			err := BlockTask("team-block-"+tt.name, "t1", "codex", "codex", "blocked")
			if err == nil {
				t.Fatalf("expected error for status %q", tt.status)
			}
			want := "not in progress"
			if tt.status == TaskStatusBlocked {
				want = "already blocked"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestClaimTask_ReclaimClearsTerminalFields_Failed(t *testing.T) {
	_, dir := setupTasksDir(t, "team-reclaim-failed")
	writeTasksJSON(t, dir, []map[string]any{
		{
			"id":             "t1",
			"title":          "Retry me",
			"status":         TaskStatusFailed,
			"assigned_to":    "codex",
			"failure_reason": "boom",
			"evidence":       map[string]any{"ci_status": "red"},
		},
	})

	if err := ClaimTask("team-reclaim-failed", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	task := wrapper["tasks"].([]any)[0].(map[string]any)
	if task["status"] != TaskStatusInProgress {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusInProgress)
	}
	if _, ok := task["failure_reason"]; ok {
		t.Errorf("failure_reason should be cleared, got %v", task["failure_reason"])
	}
	if _, ok := task["block_reason"]; ok {
		t.Errorf("block_reason should be absent, got %v", task["block_reason"])
	}
	if _, ok := task["evidence"]; ok {
		t.Errorf("evidence should be cleared, got %v", task["evidence"])
	}
}

func TestClaimTask_ReclaimClearsTerminalFields_Blocked(t *testing.T) {
	_, dir := setupTasksDir(t, "team-reclaim-blocked")
	writeTasksJSON(t, dir, []map[string]any{
		{
			"id":           "t1",
			"title":        "Retry me",
			"status":       TaskStatusBlocked,
			"assigned_to":  "codex",
			"block_reason": "waiting on API",
			"evidence":     map[string]any{"ci_status": "yellow"},
		},
	})

	if err := ClaimTask("team-reclaim-blocked", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	task := wrapper["tasks"].([]any)[0].(map[string]any)
	if task["status"] != TaskStatusInProgress {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusInProgress)
	}
	if _, ok := task["failure_reason"]; ok {
		t.Errorf("failure_reason should be absent, got %v", task["failure_reason"])
	}
	if _, ok := task["block_reason"]; ok {
		t.Errorf("block_reason should be cleared, got %v", task["block_reason"])
	}
	if _, ok := task["evidence"]; ok {
		t.Errorf("evidence should be cleared, got %v", task["evidence"])
	}
}

func TestFailTask_EmptyReasonOmitsField(t *testing.T) {
	_, dir := setupTasksDir(t, "team-fail-empty-reason")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Broken", "status": "in_progress", "assigned_to": "codex"},
	})

	if err := FailTask("team-fail-empty-reason", "t1", "codex", "codex", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	task := wrapper["tasks"].([]any)[0].(map[string]any)
	if _, ok := task["failure_reason"]; ok {
		t.Errorf("failure_reason should be omitted, got %v", task["failure_reason"])
	}
}

func TestBlockTask_EmptyReasonOmitsField(t *testing.T) {
	_, dir := setupTasksDir(t, "team-block-empty-reason")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Blocked", "status": "in_progress", "assigned_to": "codex"},
	})

	if err := BlockTask("team-block-empty-reason", "t1", "codex", "codex", ""); err != nil {
		t.Fatalf("BlockTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	task := wrapper["tasks"].([]any)[0].(map[string]any)
	if _, ok := task["block_reason"]; ok {
		t.Errorf("block_reason should be omitted, got %v", task["block_reason"])
	}
}

func TestClaimTask_NoDir_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	err := ClaimTask("nonexistent", "t1", "codex")
	if err == nil {
		t.Fatal("expected error for missing tasks dir")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClaimTask_Concurrent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("concurrency locking uses flock (darwin/linux)")
	}

	_, dir := setupTasksDir(t, "team-concurrent-claims")

	const n = 25
	tasks := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, map[string]any{
			"id":     fmt.Sprintf("t%d", i),
			"title":  fmt.Sprintf("Task %d", i),
			"status": TaskStatusPending,
		})
	}
	writeTasksJSON(t, dir, tasks)

	start := make(chan struct{})
	errCh := make(chan error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			taskID := fmt.Sprintf("t%d", i)
			if err := ClaimTask("team-concurrent-claims", taskID, "codex"); err != nil {
				errCh <- fmt.Errorf("ClaimTask(%s): %w", taskID, err)
			}
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for concurrent ClaimTask calls")
	}
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	got, err := ListTasks("team-concurrent-claims")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(tasks) = %d, want %d", len(got), n)
	}
	for _, task := range got {
		if task.Status != TaskStatusInProgress {
			t.Errorf("task %s status = %q, want %q", task.ID, task.Status, TaskStatusInProgress)
		}
		if task.AssignedTo != "codex" {
			t.Errorf("task %s assigned_to = %q, want %q", task.ID, task.AssignedTo, "codex")
		}
	}
}

// --- Round-trip preserves unknown fields ---

func TestClaimTask_PreservesUnknownFields(t *testing.T) {
	_, dir := setupTasksDir(t, "team8")

	// Write a task with fields our Task struct doesn't know about
	writeTasksJSON(t, dir, []map[string]any{
		{
			"id":            "t1",
			"title":         "Has extras",
			"status":        "pending",
			"custom_field":  "preserve_me",
			"nested":        map[string]any{"a": 1, "b": "two"},
			"numeric_thing": 42,
		},
	})

	// Claim the task (triggers read-modify-write)
	if err := ClaimTask("team8", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// Read back raw JSON and check unknown fields survived
	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}

	tasksRaw, ok := wrapper["tasks"].([]any)
	if !ok || len(tasksRaw) == 0 {
		t.Fatal("missing tasks array")
	}
	task, ok := tasksRaw[0].(map[string]any)
	if !ok {
		t.Fatal("task is not a map")
	}

	// Known fields should be updated
	if task["status"] != TaskStatusInProgress {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusInProgress)
	}
	if task["assigned_to"] != "codex" {
		t.Errorf("assigned_to = %v, want %q", task["assigned_to"], "codex")
	}

	// Unknown fields should be preserved
	if task["custom_field"] != "preserve_me" {
		t.Errorf("custom_field = %v, want %q", task["custom_field"], "preserve_me")
	}
	nested, ok := task["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested not preserved, got %T: %v", task["nested"], task["nested"])
	}
	if nested["b"] != "two" {
		t.Errorf("nested.b = %v, want %q", nested["b"], "two")
	}
	// JSON numbers are float64
	if task["numeric_thing"] != float64(42) {
		t.Errorf("numeric_thing = %v, want 42", task["numeric_thing"])
	}
}

func TestCompleteTask_PerFile_PreservesUnknownFields(t *testing.T) {
	_, dir := setupTasksDir(t, "team9")
	writeTaskFile(t, dir, "task-x.json", map[string]any{
		"id":          "x1",
		"title":       "Per-file extras",
		"status":      "in_progress",
		"assigned_to": "codex",
		"extra":       []any{"a", "b", "c"},
	})

	if err := CompleteTask("team9", "x1", "codex", "codex", nil); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "task-x.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	if raw["status"] != TaskStatusCompleted {
		t.Errorf("status = %v, want %q", raw["status"], TaskStatusCompleted)
	}
	extra, ok := raw["extra"].([]any)
	if !ok || len(extra) != 3 {
		t.Errorf("extra not preserved: %v", raw["extra"])
	}
}

func TestCompleteTask_NilEvidenceClearsExistingEvidence(t *testing.T) {
	_, dir := setupTasksDir(t, "team-complete-clear-evidence")
	writeTasksJSON(t, dir, []map[string]any{
		{
			"id":             "t1",
			"title":          "Retry complete",
			"status":         TaskStatusFailed,
			"assigned_to":    "codex",
			"failure_reason": "boom",
			"evidence":       map[string]any{"ci_status": "green"},
		},
	})

	if err := ClaimTask("team-complete-clear-evidence", "t1", "codex"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := CompleteTask("team-complete-clear-evidence", "t1", "codex", "codex", nil); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	task := wrapper["tasks"].([]any)[0].(map[string]any)
	if task["status"] != TaskStatusCompleted {
		t.Errorf("status = %v, want %q", task["status"], TaskStatusCompleted)
	}
	if _, ok := task["evidence"]; ok {
		t.Errorf("evidence should be absent after nil evidence complete, got %v", task["evidence"])
	}
}
