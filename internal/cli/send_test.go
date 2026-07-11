package cli

import (
	"os"
	"path/filepath"
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

func TestSendRootOnlyConfirmationUsesRootBasename(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-root", "clitest")
	for _, agent := range []string{"lead", "qa"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	output, err := captureEnvStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "lead", "--to", "qa", "--subject", "x", "--body", "y"})
	})
	if err != nil {
		t.Fatalf("runSend: %v", err)
	}
	if !strings.Contains(output, "session: clitest") {
		t.Fatalf("confirmation should include root basename as session label, got %q", output)
	}
	if strings.Contains(output, "session: ,") {
		t.Fatalf("confirmation should not contain a blank session label, got %q", output)
	}
}

func TestSendRootOnlyJSONUsesRootBasenameAsSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-root", "clitest")
	for _, agent := range []string{"lead", "qa"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	output := runSendJSONForTest(t, "--root", root, "--me", "lead", "--to", "qa", "--subject", "x", "--body", "y", "--json")
	if got := output["session"]; got != "clitest" {
		t.Fatalf("session = %v, want clitest", got)
	}
	if got := output["root"]; got != root {
		t.Fatalf("root = %v, want %s", got, root)
	}
}

func TestSendWaitTimeoutNamesDeliveryContextAndDoctor(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent-mail", "collab")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	for _, key := range []string{envRoot, envBaseRoot, envSession} {
		setOptionalEnv(t, key, "", false)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob",
			"--body", "timeout hint",
			"--wait-for", "drained",
			"--wait-timeout", "1ms",
		})
	})
	if err == nil || GetExitCode(err) != ExitTimeout {
		t.Fatalf("send wait should time out, got %v", err)
	}
	for _, want := range []string{root, "collab", "amq doctor --ops"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("timeout hint missing %q: %v", want, err)
		}
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
