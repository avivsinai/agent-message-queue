package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// TestSendStatusKindDefaultsToLowPriority pins the documented default: a
// status message sent with no explicit --priority carries priority low
// (CLAUDE.md message-kinds table; skills/amq-cli/references/operations.md).
// Found during the docs editorial reset on 2026-09-19: send and reply
// assigned normal to every kind, so the published contract was false.
func TestSendStatusKindDefaultsToLowPriority(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent-mail", "collab")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	for _, key := range []string{envRoot, envBaseRoot, envSession} {
		setOptionalEnv(t, key, "", false)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--kind", "status", "--body", "phase 1 done, ETA 10m"})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bob inbox/new entries=%d err=%v", len(entries), err)
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "bob"), entries[0].Name()))
	if err != nil {
		t.Fatalf("read delivered message: %v", err)
	}
	if msg.Header.Priority != format.PriorityLow {
		t.Fatalf("status priority = %q, want %q (documented default)", msg.Header.Priority, format.PriorityLow)
	}
}
