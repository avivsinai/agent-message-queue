package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendRefusesAMBaseRootOnlyCrossTreeEvidence(t *testing.T) {
	parent := t.TempDir()
	sourceBase := filepath.Join(parent, "source", ".agent-mail")
	foreignRoot := sessionRoot(t, filepath.Join(parent, "foreign"), "session1", "alice", "bob")

	t.Setenv(envRoot, foreignRoot)
	t.Setenv(envBaseRoot, sourceBase)
	setOptionalEnv(t, envSession, "", false)

	err := runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong tree"})
	assertConsumeRefused(t, err, "send")
	if got := inboxCount(t, foreignRoot, "bob"); got != 0 {
		t.Fatalf("foreign send delivered %d message(s)", got)
	}
}

func TestMutatingCommandsRefuseForeignPinnedSession(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "reply",
			run: func() error {
				return runReply([]string{"--me", "alice", "--id", "missing", "--body", "reply"})
			},
		},
		{
			name: "watch",
			run: func() error {
				_, _, err := captureEnvOutput(t, func() error {
					return runWatch([]string{"--me", "alice", "--timeout", "1ms", "--poll"})
				})
				return err
			},
		},
		{
			name: "dlq read",
			run: func() error {
				return runDLQRead([]string{"--me", "alice", "--id", "missing"})
			},
		},
		{
			name: "dlq retry",
			run: func() error {
				return runDLQRetry([]string{"--me", "alice", "--id", "missing"})
			},
		},
		{
			name: "dlq purge",
			run: func() error {
				return runDLQPurge([]string{"--me", "alice", "--yes"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			baseRoot := filepath.Join(parent, ".agent-mail")
			_ = sessionRoot(t, parent, "session1", "alice", "bob")
			foreignRoot := sessionRoot(t, parent, "session2", "alice", "bob")

			t.Setenv(envRoot, foreignRoot)
			t.Setenv(envBaseRoot, baseRoot)
			t.Setenv(envSession, "session1")

			err := tt.run()
			if err == nil || GetExitCode(err) != ExitContextMismatch {
				t.Fatalf("%s should refuse foreign pinned session, got %v", tt.name, err)
			}
		})
	}
}

func TestWatchMissingMailboxDoesNotCreateIt(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := filepath.Join(baseRoot, "session1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "session1")

	_, _, err := captureEnvOutput(t, func() error {
		return runWatch([]string{"--me", "alice", "--timeout", "1ms", "--poll"})
	})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing watch mailbox should be not-found, got %v", err)
	}
	if _, statErr := os.Stat(fsq.AgentInboxNew(root, "alice")); !os.IsNotExist(statErr) {
		t.Fatalf("watch created missing inbox, stat error = %v", statErr)
	}
}
