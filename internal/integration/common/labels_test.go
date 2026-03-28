package common

import (
	"strings"
	"testing"
)

func TestBuildOrchestratorLabels_Basic(t *testing.T) {
	labels := BuildOrchestratorLabels("symphony", "running")

	expected := []string{"orchestrator", "orchestrator:symphony", "task-state:running"}
	if len(labels) != len(expected) {
		t.Fatalf("expected %d labels, got %d: %v", len(expected), len(labels), labels)
	}
	for i, want := range expected {
		if labels[i] != want {
			t.Errorf("labels[%d] = %q, want %q", i, labels[i], want)
		}
	}
}

func TestBuildOrchestratorLabels_NoState(t *testing.T) {
	labels := BuildOrchestratorLabels("kanban", "")

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(labels), labels)
	}
	if labels[0] != "orchestrator" {
		t.Errorf("expected orchestrator, got %s", labels[0])
	}
	if labels[1] != "orchestrator:kanban" {
		t.Errorf("expected orchestrator:kanban, got %s", labels[1])
	}
}

func TestBuildOrchestratorLabels_WithFlags(t *testing.T) {
	labels := BuildOrchestratorLabels("kanban", "awaiting_review", "handoff", "blocking")

	found := strings.Join(labels, ",")
	for _, want := range []string{"orchestrator", "orchestrator:kanban", "task-state:awaiting_review", "handoff", "blocking"} {
		if !strings.Contains(found, want) {
			t.Errorf("expected %q in labels %v", want, labels)
		}
	}
}

func TestBuildOrchestratorLabels_EmptyFlags(t *testing.T) {
	// Empty string flags should be filtered
	labels := BuildOrchestratorLabels("symphony", "running", "", "handoff", "")

	for _, l := range labels {
		if l == "" {
			t.Error("expected no empty labels")
		}
	}

	if len(labels) != 4 { // orchestrator, orchestrator:symphony, task-state:running, handoff
		t.Errorf("expected 4 labels, got %d: %v", len(labels), labels)
	}
}
