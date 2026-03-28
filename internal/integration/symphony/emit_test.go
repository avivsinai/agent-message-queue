package symphony

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// setupAMQRoot creates a minimal AMQ root with agent inbox dirs.
func setupAMQRoot(t *testing.T, agent string) string {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEmit_BasicEvent(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event:      "after_create",
		Me:         "codex",
		Root:       root,
		Workspace:  "/tmp/test-workspace",
		Identifier: "MT-42",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if result.Event != "after_create" {
		t.Errorf("expected event=after_create, got %s", result.Event)
	}
	if result.Me != "codex" {
		t.Errorf("expected me=codex, got %s", result.Me)
	}
	if result.Workspace != "/tmp/test-workspace" {
		t.Errorf("expected workspace=/tmp/test-workspace, got %s", result.Workspace)
	}
	if result.Identifier != "mt-42" { // lowercased by sanitize
		t.Errorf("expected identifier=mt-42, got %s", result.Identifier)
	}
	if result.Thread != "task/mt-42" {
		t.Errorf("expected thread=task/mt-42, got %s", result.Thread)
	}

	// Verify message was delivered
	if result.MessagePath == "" {
		t.Fatal("expected non-empty message path")
	}
	if _, err := os.Stat(result.MessagePath); err != nil {
		t.Fatalf("message file does not exist: %v", err)
	}

	// Parse the delivered message
	msg, err := format.ReadMessageFile(result.MessagePath)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	if msg.Header.From != "codex" {
		t.Errorf("expected from=codex, got %s", msg.Header.From)
	}
	if len(msg.Header.To) != 1 || msg.Header.To[0] != "codex" {
		t.Errorf("expected to=[codex], got %v", msg.Header.To)
	}
	if msg.Header.Thread != "task/mt-42" {
		t.Errorf("expected thread=task/mt-42, got %s", msg.Header.Thread)
	}
	if msg.Header.Kind != format.KindStatus {
		t.Errorf("expected kind=status, got %s", msg.Header.Kind)
	}

	// Verify context has orchestrator key
	if msg.Header.Context == nil {
		t.Fatal("expected non-nil context")
	}
	orchRaw, ok := msg.Header.Context["orchestrator"]
	if !ok {
		t.Fatal("expected orchestrator key in context")
	}

	// Verify orchestrator structure
	orch, ok := orchRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected orchestrator to be a map, got %T", orchRaw)
	}
	if orch["name"] != "symphony" {
		t.Errorf("expected name=symphony, got %v", orch["name"])
	}
	if orch["transport"] != "hook" {
		t.Errorf("expected transport=hook, got %v", orch["transport"])
	}
	if orch["event"] != "after_create" {
		t.Errorf("expected event=after_create, got %v", orch["event"])
	}
}

func TestEmit_AllEvents(t *testing.T) {
	for _, event := range ValidEvents {
		t.Run(event, func(t *testing.T) {
			root := setupAMQRoot(t, "codex")

			result, err := Emit(EmitOptions{
				Event:      event,
				Me:         "codex",
				Root:       root,
				Workspace:  "/tmp/ws",
				Identifier: "test-id",
			})
			if err != nil {
				t.Fatalf("Emit %s: %v", event, err)
			}

			if result.Event != event {
				t.Errorf("expected event=%s, got %s", event, result.Event)
			}
		})
	}
}

func TestEmit_InvalidEvent(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	_, err := Emit(EmitOptions{
		Event:     "invalid_event",
		Me:        "codex",
		Root:      root,
		Workspace: "/tmp/ws",
	})
	if err == nil {
		t.Fatal("expected error for invalid event")
	}
}

func TestEmit_DefaultWorkspace(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event: "after_create",
		Me:    "codex",
		Root:  root,
		// No workspace specified - should default to cwd
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	cwd, _ := os.Getwd()
	if result.Workspace != cwd {
		t.Errorf("expected workspace=%s, got %s", cwd, result.Workspace)
	}
}

func TestEmit_DefaultIdentifier(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event:     "after_create",
		Me:        "codex",
		Root:      root,
		Workspace: "/tmp/my-workspace",
		// No identifier - should default to basename
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if result.Identifier != "my-workspace" {
		t.Errorf("expected identifier=my-workspace, got %s", result.Identifier)
	}
}

func TestEmit_Labels(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event:      "after_create",
		Me:         "codex",
		Root:       root,
		Workspace:  "/tmp/ws",
		Identifier: "test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	msg, err := format.ReadMessageFile(result.MessagePath)
	if err != nil {
		t.Fatal(err)
	}

	labels := msg.Header.Labels
	hasOrchestrator := false
	hasSymphony := false
	hasState := false
	for _, l := range labels {
		if l == "orchestrator" {
			hasOrchestrator = true
		}
		if l == "orchestrator:symphony" {
			hasSymphony = true
		}
		if strings.HasPrefix(l, "task-state:") {
			hasState = true
		}
	}

	if !hasOrchestrator {
		t.Error("expected 'orchestrator' label")
	}
	if !hasSymphony {
		t.Error("expected 'orchestrator:symphony' label")
	}
	if !hasState {
		t.Error("expected 'task-state:*' label")
	}
}

func TestEmit_ContextJSON(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event:      "before_run",
		Me:         "codex",
		Root:       root,
		Workspace:  "/tmp/ws",
		Identifier: "test-id",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	msg, err := format.ReadMessageFile(result.MessagePath)
	if err != nil {
		t.Fatal(err)
	}

	// Serialize and deserialize context to verify structure
	ctxBytes, err := json.Marshal(msg.Header.Context)
	if err != nil {
		t.Fatal(err)
	}

	var ctx map[string]interface{}
	if err := json.Unmarshal(ctxBytes, &ctx); err != nil {
		t.Fatal(err)
	}

	orch, ok := ctx["orchestrator"].(map[string]interface{})
	if !ok {
		t.Fatal("expected orchestrator in context")
	}

	// Check version
	if v, ok := orch["version"].(float64); !ok || v != 1 {
		t.Errorf("expected version=1, got %v", orch["version"])
	}

	// Check workspace
	ws, ok := orch["workspace"].(map[string]interface{})
	if !ok {
		t.Fatal("expected workspace in orchestrator")
	}
	if ws["path"] != "/tmp/ws" {
		t.Errorf("expected workspace.path=/tmp/ws, got %v", ws["path"])
	}
	if ws["key"] != "test-id" {
		t.Errorf("expected workspace.key=test-id, got %v", ws["key"])
	}

	// Check task
	task, ok := orch["task"].(map[string]interface{})
	if !ok {
		t.Fatal("expected task in orchestrator")
	}
	if task["id"] != "test-id" {
		t.Errorf("expected task.id=test-id, got %v", task["id"])
	}
	if task["state"] != "running" {
		t.Errorf("expected task.state=running for before_run, got %v", task["state"])
	}
}

func TestEmit_MessageInInbox(t *testing.T) {
	root := setupAMQRoot(t, "codex")

	result, err := Emit(EmitOptions{
		Event:      "after_create",
		Me:         "codex",
		Root:       root,
		Workspace:  "/tmp/ws",
		Identifier: "test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Verify message is in the inbox/new directory
	newDir := fsq.AgentInboxNew(root, "codex")
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 message in inbox/new, got %d", len(entries))
	}

	expectedPath := filepath.Join(newDir, entries[0].Name())
	if result.MessagePath != expectedPath {
		t.Errorf("expected message path=%s, got %s", expectedPath, result.MessagePath)
	}
}

func TestSanitizeIdentifier(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"MT-42", "mt-42"},
		{"simple", "simple"},
		{"UPPER", "upper"},
		{"with spaces", "with-spaces"},
		{"special!@#chars", "special---chars"},
		{"my_workspace", "my_workspace"},
		{"  trimmed  ", "trimmed"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeIdentifier(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeIdentifier(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestValidateEvent(t *testing.T) {
	for _, event := range ValidEvents {
		if err := validateEvent(event); err != nil {
			t.Errorf("expected valid event %q, got error: %v", event, err)
		}
	}

	if err := validateEvent("invalid"); err == nil {
		t.Error("expected error for invalid event")
	}
}

func TestEventToTaskState(t *testing.T) {
	tests := []struct {
		event string
		state string
	}{
		{"after_create", "created"},
		{"before_run", "running"},
		{"after_run", "completed"},
		{"before_remove", "removing"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		got := eventToTaskState(tt.event)
		if got != tt.state {
			t.Errorf("eventToTaskState(%q) = %q, want %q", tt.event, got, tt.state)
		}
	}
}
