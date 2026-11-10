package common

import (
	"errors"
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
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

// Integration delivery wrote into any root while the shell was pinned to
// another session; it now refuses as the send path does (audit 2026-11-10).
func TestDeliverIntegrationMessageRefusesARootOutsideTheSessionPin(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionguard.EnvBaseRoot, t.TempDir())
	t.Setenv(sessionguard.EnvSession, "s1")
	_, err := DeliverIntegrationMessage(root, "codex", "codex", "s", "b", nil, nil, "task/t", format.KindStatus, format.PriorityLow)
	var refusal *sessionguard.ContextError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v; want a session context refusal", err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex")); len(entries) != 0 {
		t.Fatalf("inbox holds %d messages; want none", len(entries))
	}
}
