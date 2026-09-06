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

func TestClaimTask_PerFileLayout(t *testing.T) {
	_, dir := setupTasksDir(t, "team-perfile")
	writeTaskFile(t, dir, "my-task.json", map[string]any{
		"id": "mt1", "title": "Per-file task", "status": "pending",
	})

	if err := ClaimTask("team-perfile", "mt1", "codex"); err != nil {
		t.Fatalf("ClaimTask per-file: %v", err)
	}

	tasks, err := ListTasks("team-perfile")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != TaskStatusInProgress || tasks[0].AssignedTo != "codex" {
		t.Fatalf("per-file claim result = %#v, want in_progress assigned to codex", tasks)
	}
}
