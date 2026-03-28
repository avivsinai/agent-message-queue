package common

import (
	"testing"
)

func TestBuildOrchestratorContext_Full(t *testing.T) {
	ws := &WorkspaceContext{
		Path: "/tmp/workspace",
		Key:  "mt-42",
	}
	task := map[string]interface{}{
		"id":    "mt-42",
		"state": "running",
	}

	ctx := BuildOrchestratorContext("symphony", "hook", "before_run", ws, task)

	orch, ok := ctx["orchestrator"].(map[string]interface{})
	if !ok {
		t.Fatal("expected orchestrator key in context")
	}

	if orch["version"] != 1 {
		t.Errorf("expected version=1, got %v", orch["version"])
	}
	if orch["name"] != "symphony" {
		t.Errorf("expected name=symphony, got %v", orch["name"])
	}
	if orch["transport"] != "hook" {
		t.Errorf("expected transport=hook, got %v", orch["transport"])
	}
	if orch["event"] != "before_run" {
		t.Errorf("expected event=before_run, got %v", orch["event"])
	}

	wsMap, ok := orch["workspace"].(map[string]interface{})
	if !ok {
		t.Fatal("expected workspace in orchestrator")
	}
	if wsMap["path"] != "/tmp/workspace" {
		t.Errorf("expected workspace.path=/tmp/workspace, got %v", wsMap["path"])
	}
	if wsMap["key"] != "mt-42" {
		t.Errorf("expected workspace.key=mt-42, got %v", wsMap["key"])
	}

	taskMap, ok := orch["task"].(map[string]interface{})
	if !ok {
		t.Fatal("expected task in orchestrator")
	}
	if taskMap["id"] != "mt-42" {
		t.Errorf("expected task.id=mt-42, got %v", taskMap["id"])
	}
}

func TestBuildOrchestratorContext_NilOptionals(t *testing.T) {
	ctx := BuildOrchestratorContext("kanban", "bridge", "task_ready_for_review", nil, nil)

	orch, ok := ctx["orchestrator"].(map[string]interface{})
	if !ok {
		t.Fatal("expected orchestrator key")
	}

	if orch["name"] != "kanban" {
		t.Errorf("expected name=kanban, got %v", orch["name"])
	}
	if _, exists := orch["workspace"]; exists {
		t.Error("expected no workspace key when nil")
	}
	if _, exists := orch["task"]; exists {
		t.Error("expected no task key when nil")
	}
}
