package swarm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	if err := CompleteTask("team6", "t1", "codex"); err != nil {
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

	err := CompleteTask("team7", "t1", "codex")
	if err == nil {
		t.Fatal("expected error for wrong agent")
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

	if err := CompleteTask("team9", "x1", "codex"); err != nil {
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
