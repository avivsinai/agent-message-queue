package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunAckRejectsInvalidHeader(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs claude: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "bad/../id",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Created: "2025-12-24T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "bad.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad"}); err == nil {
		t.Fatalf("expected error for invalid message id in header")
	}

	msg.Header.ID = "msg-1"
	msg.Header.From = "CloudCode"
	data, err = msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	badPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad-from.md")
	if _, err := fsq.WriteFileAtomic(filepath.Dir(badPath), filepath.Base(badPath), data, 0o644); err != nil {
		t.Fatalf("write bad-from: %v", err)
	}
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad-from"}); err == nil {
		t.Fatalf("expected error for invalid sender handle in header")
	}
}

func TestRunAckCorruptAckFileRecovery(t *testing.T) {
	t.Setenv("AM_ROOT", "") // Clear to avoid guardRootOverride conflict with --root
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs claude: %v", err)
	}

	// Create a valid message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-recover",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Created: "2025-12-25T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-recover.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Create a corrupt ack file
	ackDir := fsq.AgentAcksSent(root, "codex")
	if err := os.MkdirAll(ackDir, 0o700); err != nil {
		t.Fatalf("mkdir ack dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "msg-recover.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt ack: %v", err)
	}

	// Ack should succeed (recover from corrupt file)
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "msg-recover"}); err != nil {
		t.Fatalf("ack should recover from corrupt file: %v", err)
	}

	// Verify the ack file was rewritten
	ackData, err := os.ReadFile(filepath.Join(ackDir, "msg-recover.json"))
	if err != nil {
		t.Fatalf("read ack file: %v", err)
	}
	if string(ackData) == "not json" {
		t.Errorf("ack file should have been rewritten")
	}
}
