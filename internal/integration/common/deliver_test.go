package common

import (
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDeliverIntegrationMessage(t *testing.T) {
	root := t.TempDir()
	agent := "codex"
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}

	ctx := map[string]interface{}{
		"orchestrator": map[string]interface{}{
			"version": 1,
			"name":    "symphony",
		},
	}
	labels := []string{"orchestrator", "orchestrator:symphony"}

	path, err := DeliverIntegrationMessage(
		root, agent, agent,
		"[symphony] after_create: test",
		"Event: after_create\n",
		ctx, labels,
		"task/test", format.KindStatus, format.PriorityLow,
	)
	if err != nil {
		t.Fatalf("DeliverIntegrationMessage: %v", err)
	}

	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("delivered message not found: %v", err)
	}

	// Parse and verify
	msg, err := format.ReadMessageFile(path)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	if msg.Header.From != agent {
		t.Errorf("expected from=%s, got %s", agent, msg.Header.From)
	}
	if msg.Header.Thread != "task/test" {
		t.Errorf("expected thread=task/test, got %s", msg.Header.Thread)
	}
	if msg.Header.Kind != format.KindStatus {
		t.Errorf("expected kind=status, got %s", msg.Header.Kind)
	}
	if msg.Header.Priority != format.PriorityLow {
		t.Errorf("expected priority=low, got %s", msg.Header.Priority)
	}
	if len(msg.Header.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(msg.Header.Labels))
	}
}

func TestDeliverIntegrationMessage_MissingInbox(t *testing.T) {
	root := t.TempDir()
	// Don't create agent dirs

	_, err := DeliverIntegrationMessage(
		root, "codex", "codex",
		"test", "body",
		nil, nil,
		"task/test", format.KindStatus, format.PriorityLow,
	)
	// DeliverToInboxes creates dirs via MkdirAll, so this should succeed
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
