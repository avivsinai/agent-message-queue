package swarm

import (
	"testing"
)

func TestPollForChanges_NewTask(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Existing", "status": "pending"},
	})

	lastStates := make(map[string]string)
	cfg := BridgeConfig{
		TeamName:    "bridge-team",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Type != "task_added" {
		t.Errorf("event type = %q, want %q", events[0].Type, "task_added")
	}
	if events[0].TaskID != "t1" {
		t.Errorf("task id = %q, want %q", events[0].TaskID, "t1")
	}
}

func TestPollForChanges_NoChange(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team2")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Stable", "status": "pending"},
	})

	lastStates := map[string]string{"t1": "pending:"}
	cfg := BridgeConfig{
		TeamName:    "bridge-team2",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestPollForChanges_TaskAssigned(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team3")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Assigned", "status": "in_progress", "assigned_to": "codex"},
	})

	lastStates := map[string]string{"t1": "pending:"}
	cfg := BridgeConfig{
		TeamName:    "bridge-team3",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Type != "task_assigned" {
		t.Errorf("event type = %q, want %q", events[0].Type, "task_assigned")
	}
}

func TestPollForChanges_TaskCompleted(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team4")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Done", "status": "completed", "assigned_to": "codex"},
	})

	lastStates := map[string]string{"t1": "in_progress:codex"}
	cfg := BridgeConfig{
		TeamName:    "bridge-team4",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Type != "task_completed" {
		t.Errorf("event type = %q, want %q", events[0].Type, "task_completed")
	}
}

func TestPollForChanges_IrrelevantTask(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team5")
	// Task assigned to someone else — not relevant
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Not mine", "status": "in_progress", "assigned_to": "other-agent"},
	})

	lastStates := make(map[string]string)
	cfg := BridgeConfig{
		TeamName:    "bridge-team5",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (task assigned to other agent)", len(events))
	}
}

func TestPollForChanges_RemovedTask(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team6")
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Survivor", "status": "pending"},
	})

	// t2 was known but is now gone
	lastStates := map[string]string{
		"t1": "pending:",
		"t2": "pending:",
	}
	cfg := BridgeConfig{
		TeamName:    "bridge-team6",
		AgentHandle: "codex",
	}

	_, err := pollForChanges(cfg, lastStates)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if _, ok := lastStates["t2"]; ok {
		t.Error("t2 should have been removed from lastStates")
	}
	if _, ok := lastStates["t1"]; !ok {
		t.Error("t1 should still be in lastStates")
	}
}

func TestIsRelevantToAgent(t *testing.T) {
	tests := []struct {
		name     string
		task     Task
		handle   string
		agentID  string
		relevant bool
	}{
		{"unassigned", Task{AssignedTo: ""}, "codex", "ext_1", true},
		{"assigned to me", Task{AssignedTo: "codex"}, "codex", "ext_1", true},
		{"assigned by id", Task{AssignedTo: "ext_1"}, "codex", "ext_1", true},
		{"assigned to other", Task{AssignedTo: "claude"}, "codex", "ext_1", false},
		{"contains handle", Task{AssignedTo: "codex-worker"}, "codex", "ext_1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRelevantToAgent(tt.task, tt.handle, tt.agentID)
			if got != tt.relevant {
				t.Errorf("isRelevantToAgent = %v, want %v", got, tt.relevant)
			}
		})
	}
}
