package cli

import (
	"path/filepath"
	"testing"
)

func TestParticipatingCommandsRefusePinnedRootWhenCwdHasRepoLocalQueue(t *testing.T) {
	tests := []struct {
		name    string
		command string
		run     func() error
	}{
		{"send", "send", func() error {
			return runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong queue"})
		}},
		{"reply", "reply", func() error {
			return runReply([]string{"--me", "alice", "--id", "missing", "--body", "wrong queue"})
		}},
		{"read", "read", func() error {
			return runRead([]string{"--me", "alice", "--id", "missing"})
		}},
		{"drain", "drain", func() error {
			return runDrain([]string{"--me", "alice"})
		}},
		{"watch", "watch", func() error {
			return runWatch([]string{"--me", "alice", "--poll", "--timeout", "1ms"})
		}},
		{"monitor", "monitor", func() error {
			return runMonitor([]string{"--me", "alice", "--poll", "--timeout", "1ms"})
		}},
		{"dlq list", "dlq list", func() error {
			return runDLQList([]string{"--me", "alice", "--json"})
		}},
		{"dlq read", "dlq read", func() error {
			return runDLQRead([]string{"--me", "alice", "--id", "missing"})
		}},
		{"dlq retry", "dlq retry", func() error {
			return runDLQRetry([]string{"--me", "alice", "--id", "missing"})
		}},
		{"dlq purge", "dlq purge", func() error {
			return runDLQPurge([]string{"--me", "alice", "--yes"})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			globalProject := filepath.Join(parent, "global")
			localProject := filepath.Join(parent, "local")
			globalBase := filepath.Join(globalProject, ".agent-mail")
			globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
			_ = sessionRoot(t, localProject, "session1", "alice", "bob")

			t.Chdir(localProject)
			pinSendSessionForTest(t, globalBase, globalRoot, "session1")

			stdout, _, err := captureEnvOutput(t, test.run)
			assertConsumeRefused(t, err, test.command)
			if stdout != "" {
				t.Fatalf("refused %s emitted stdout: %q", test.command, stdout)
			}
		})
	}
}
