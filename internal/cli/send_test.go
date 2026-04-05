package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSend_RejectsPositionalArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "body as positional arg",
			args: []string{"--me", "alice", "--to", "bob", "my message body"},
		},
		{
			name: "multiple positional args",
			args: []string{"--me", "alice", "--to", "bob", "hello", "world"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, agent := range []string{"alice", "bob"} {
				if err := fsq.EnsureAgentDirs(root, agent); err != nil {
					t.Fatalf("EnsureAgentDirs: %v", err)
				}
			}

			args := append([]string{"--root", root}, tt.args...)
			err := runSend(args)
			if err == nil {
				t.Fatal("expected error for positional arguments, got nil")
			}
			if code := GetExitCode(err); code != ExitUsage {
				t.Fatalf("exit code = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(err.Error(), "does not accept positional arguments") {
				t.Errorf("expected positional args error, got: %v", err)
			}
			if !strings.Contains(err.Error(), "--body") {
				t.Errorf("error should suggest --body, got: %v", err)
			}

			entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("expected bob inbox to remain empty, got %d message(s)", len(entries))
			}
		})
	}
}

func TestReply_RejectsPositionalArgs(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	now := time.Now()
	originalID, err := format.NewMessageID(now)
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	originalMsg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Question about code",
			Created: now.UTC().Format(time.RFC3339Nano),
			Kind:    format.KindQuestion,
		},
		Body: "How does this work?",
	}
	data, err := originalMsg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := fsq.DeliverToInboxes(root, []string{"alice"}, originalID+".md", data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	err = runReply([]string{"--me", "alice", "--id", originalID, "--root", root, "my reply body"})
	if err == nil {
		t.Fatal("expected error for positional arguments, got nil")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "does not accept positional arguments") {
		t.Errorf("expected positional args error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--body") {
		t.Errorf("error should suggest --body, got: %v", err)
	}

	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected bob inbox to remain empty, got %d message(s)", len(entries))
	}
}
