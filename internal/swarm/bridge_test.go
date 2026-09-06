package swarm

import (
	"os"
	"path/filepath"
	"strings"
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

func TestPollForChanges_TaskCompleted(t *testing.T) {
	_, dir := setupTasksDir(t, "bridge-team4")
	writeTasksJSON(t, dir, []map[string]any{
		{
			"id":          "t1",
			"title":       "Done",
			"status":      "completed",
			"assigned_to": "codex",
			"evidence": map[string]any{
				"ci_status": "green",
			},
		},
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
	if events[0].Evidence["ci_status"] != "green" {
		t.Errorf("evidence.ci_status = %v, want %q", events[0].Evidence["ci_status"], "green")
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

func TestDeliverBridgeEvent_CompletionIncludesEvidence(t *testing.T) {
	root := t.TempDir()
	cfg := BridgeConfig{
		TeamName:    "bridge-team-evidence",
		AgentHandle: "codex",
		AgentID:     "ext_codex_1",
		AMQRoot:     root,
	}

	event := BridgeEvent{
		Type:   "task_completed",
		TaskID: "t2",
		Title:  "Ship it",
		Status: TaskStatusCompleted,
		Evidence: map[string]any{
			"tests_passed": true,
			"ci_status":    "green",
		},
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

	evidence, ok := msg.Header.Context["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("context evidence missing or wrong type: %T", msg.Header.Context["evidence"])
	}
	if evidence["ci_status"] != "green" {
		t.Errorf("context evidence ci_status = %v, want %q", evidence["ci_status"], "green")
	}
	if !strings.Contains(msg.Body, "Evidence:") {
		t.Fatalf("expected evidence block in body, got %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "\"ci_status\": \"green\"") {
		t.Fatalf("expected serialized evidence in body, got %q", msg.Body)
	}
}

// Failed/blocked are user-visible transitions with a forwarded reason; keep
// one compact table proving both the emitted event and the delivered AMQ
// message carry the reason.
func TestPollForChanges_FailedAndBlockedEmitReason(t *testing.T) {
	for _, test := range []struct {
		name      string
		team      string
		status    string
		reasonKey string
		reason    string
		wantType  string
	}{
		{name: "failed", team: "bridge-fail-reason", status: TaskStatusFailed, reasonKey: "failure_reason", reason: "tests are red", wantType: "task_failed"},
		{name: "blocked", team: "bridge-block-reason", status: TaskStatusBlocked, reasonKey: "block_reason", reason: "waiting on API", wantType: "task_blocked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, dir := setupTasksDir(t, test.team)
			writeTasksJSON(t, dir, []map[string]any{
				{"id": "t1", "title": "Task", "status": test.status, "assigned_to": "codex", test.reasonKey: test.reason},
			})
			lastStates := map[string]string{"t1": "in_progress:codex"}
			cfg := BridgeConfig{TeamName: test.team, AgentHandle: "codex", AgentID: "ext_codex_1"}

			events, err := pollForChanges(cfg, lastStates, make(map[string]bool))
			if err != nil {
				t.Fatalf("pollForChanges: %v", err)
			}
			if len(events) != 1 || events[0].Type != test.wantType || events[0].Reason != test.reason {
				t.Fatalf("events = %#v, want one %q with reason %q", events, test.wantType, test.reason)
			}

			root := t.TempDir()
			cfg.AMQRoot = root
			event := events[0]
			event.TaskID = "t1"
			event.Title = "Task"
			event.Status = test.status
			if err := deliverBridgeEvent(cfg, event); err != nil {
				t.Fatalf("deliverBridgeEvent: %v", err)
			}
			newDir := fsq.AgentInboxNew(root, "codex")
			entries, err := os.ReadDir(newDir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("delivered messages = %d, want 1", len(entries))
			}
			msg, err := format.ReadMessageFile(filepath.Join(newDir, entries[0].Name()))
			if err != nil {
				t.Fatalf("ReadMessageFile: %v", err)
			}
			if msg.Header.Context["reason"] != test.reason || !strings.Contains(msg.Body, "Reason: "+test.reason) {
				t.Fatalf("delivered reason = context:%v body:%q, want %q in both", msg.Header.Context["reason"], msg.Body, test.reason)
			}
		})
	}
}
