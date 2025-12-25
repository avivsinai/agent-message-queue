package cli

import (
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
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "bad/../id",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
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
