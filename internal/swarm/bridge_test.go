package swarm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

func TestPollForChanges_TaskAssignedByAgentID(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team3b")
	// Task assigned using agent_id, not handle
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Assigned by ID", "status": "in_progress", "assigned_to": "ext_codex_1"},
	})

	lastStates := map[string]string{"t1": "pending:"}
	cfg := BridgeConfig{
		TeamName:    "bridge-team3b",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

	events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (task assigned to other agent)", len(events))
	}
}

func TestPollForChanges_NewTaskWithSatisfiedDeps_NoDoubleEmit(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team-nodbl")
	// Brand-new task with deps already satisfied — should emit task_added only, not task_unblocked
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Done", "status": "completed"},
		{"id": "t2", "title": "New ready", "status": "pending", "depends_on": []any{"t1"}},
	})

	lastStates := make(map[string]string)
	lastDepsSatisfied := make(map[string]bool)
	cfg := BridgeConfig{
		TeamName:    "bridge-team-nodbl",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates, lastDepsSatisfied)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}

	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	// Should have task_added for both, but NOT task_unblocked for t2
	for _, e := range events {
		if e.Type == "task_unblocked" {
			t.Errorf("unexpected task_unblocked for %s; new tasks should not emit unblocked (got events: %v)", e.TaskID, types)
		}
	}
	// But lastDepsSatisfied should still be populated for next cycle
	if !lastDepsSatisfied["t2"] {
		t.Error("lastDepsSatisfied[t2] should be true (deps are satisfied)")
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

	_, err := pollForChanges(cfg, lastStates, make(map[string]bool))
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

func TestPollForChanges_TaskUnblockedByDependency(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-unblock1")
	// t1 is completed, t2 depends on t1 and is pending → deps satisfied
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Prereq", "status": "completed"},
		{"id": "t2", "title": "Blocked", "status": "pending", "depends_on": []any{"t1"}},
	})

	lastStates := map[string]string{
		"t1": "in_progress:", // was in_progress, now completed
		"t2": "pending:",
	}
	// t2's deps were NOT satisfied previously
	lastDepsSatisfied := map[string]bool{
		"t1": true,
		"t2": false,
	}
	cfg := BridgeConfig{
		TeamName:    "bridge-unblock1",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates, lastDepsSatisfied)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}

	// Should have task_completed for t1 and task_unblocked for t2
	var hasCompleted, hasUnblocked bool
	for _, e := range events {
		if e.TaskID == "t1" && e.Type == "task_completed" {
			hasCompleted = true
		}
		if e.TaskID == "t2" && e.Type == "task_unblocked" {
			hasUnblocked = true
		}
	}
	if !hasCompleted {
		t.Error("expected task_completed event for t1")
	}
	if !hasUnblocked {
		t.Error("expected task_unblocked event for t2")
	}
	// lastDepsSatisfied should now be true for t2
	if !lastDepsSatisfied["t2"] {
		t.Error("lastDepsSatisfied[t2] should be true after unblocking")
	}
}

func TestPollForChanges_TaskStillBlocked(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-unblock2")
	// t2 depends on t1 and t3; t1 is completed but t3 is still pending
	writeTasksJSON(t, dir, []map[string]any{
		{"id": "t1", "title": "Done", "status": "completed"},
		{"id": "t2", "title": "Blocked", "status": "pending", "depends_on": []any{"t1", "t3"}},
		{"id": "t3", "title": "Not done", "status": "in_progress"},
	})

	lastStates := map[string]string{
		"t1": "completed:",
		"t2": "pending:",
		"t3": "in_progress:",
	}
	lastDepsSatisfied := map[string]bool{
		"t1": true,
		"t2": false,
		"t3": true,
	}
	cfg := BridgeConfig{
		TeamName:    "bridge-unblock2",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
	}

	events, err := pollForChanges(cfg, lastStates, lastDepsSatisfied)
	if err != nil {
		t.Fatalf("pollForChanges: %v", err)
	}

	// No unblocked event — t3 is still not completed
	for _, e := range events {
		if e.Type == "task_unblocked" {
			t.Errorf("unexpected task_unblocked event for %s", e.TaskID)
		}
	}
	if lastDepsSatisfied["t2"] {
		t.Error("lastDepsSatisfied[t2] should still be false")
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
		{"different handle", Task{AssignedTo: "codex-worker"}, "codex", "ext_1", false},
		{"case insensitive handle", Task{AssignedTo: "Codex"}, "codex", "ext_1", true},
		{"case insensitive id", Task{AssignedTo: "EXT_1"}, "codex", "ext_1", true},
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

func TestDeliverBridgeEvent_DeliversMessage(t *testing.T) {
	root := t.TempDir()
	cfg := BridgeConfig{
		TeamName:    "bridge-team-delivery",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
		AMQRoot:     root,
	}

	event := BridgeEvent{
		Type:    "task_assigned",
		TaskID:  "t1",
		Title:   "Do the thing",
		Status:  TaskStatusInProgress,
		Details: "assigned_to=ext_codex_1",
	}

	if err := deliverBridgeEvent(cfg, event); err != nil {
		t.Fatalf("deliverBridgeEvent: %v", err)
	}

	newDir := fsq.AgentInboxNew(root, "codex")
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", newDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	msgPath := filepath.Join(newDir, entries[0].Name())
	msg, err := format.ReadMessageFile(msgPath)
	if err != nil {
		t.Fatalf("ReadMessageFile: %v", err)
	}

	if msg.Header.From != "codex" {
		t.Errorf("Header.From = %q, want %q", msg.Header.From, "codex")
	}
	if len(msg.Header.To) != 1 || msg.Header.To[0] != "codex" {
		t.Errorf("Header.To = %v, want [%q]", msg.Header.To, "codex")
	}
	if msg.Header.Thread != "swarm/bridge-team-delivery" {
		t.Errorf("Header.Thread = %q, want %q", msg.Header.Thread, "swarm/bridge-team-delivery")
	}
	if msg.Header.Kind != format.KindStatus {
		t.Errorf("Header.Kind = %q, want %q", msg.Header.Kind, format.KindStatus)
	}
	if msg.Header.Priority != format.PriorityNormal {
		t.Errorf("Header.Priority = %q, want %q", msg.Header.Priority, format.PriorityNormal)
	}

	if msg.Header.Context["team"] != "bridge-team-delivery" {
		t.Errorf("Context.team = %v, want %q", msg.Header.Context["team"], "bridge-team-delivery")
	}
	if msg.Header.Context["task_id"] != "t1" {
		t.Errorf("Context.task_id = %v, want %q", msg.Header.Context["task_id"], "t1")
	}
	if msg.Header.Context["event"] != "task_assigned" {
		t.Errorf("Context.event = %v, want %q", msg.Header.Context["event"], "task_assigned")
	}

	if msg.Body == "" {
		t.Fatal("expected non-empty body")
	}
}
