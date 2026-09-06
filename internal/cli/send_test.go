package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendRootOnlyJSONUsesRootBasenameAsSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-root", "clitest")
	for _, agent := range []string{"lead", "qa"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	configureSendTestRoot(t, root, "lead", "qa")

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
	configureSendTestRoot(t, root, "alice", "bob")
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
	for _, want := range []string{root, "collab", "amq doctor --root", "--ops"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("timeout hint missing %q: %v", want, err)
		}
	}
}
