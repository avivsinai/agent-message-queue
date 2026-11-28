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

// A question to the owner may carry its options in context.decision; send
// refuses a shape the board would not show and delivers a valid one.
func TestSendValidatesContextDecision(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"lead", "user"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	configureSendTestRoot(t, root, "lead", "user")
	const valid = `{"decision":{"options":[{"label":"Opt-in"},{"label":"On by default"}],"recommended":"Opt-in","blocking":false}}`
	for _, tc := range []struct{ name, context, field string }{
		{"recommended not among the labels", strings.Replace(valid, `"recommended":"Opt-in"`, `"recommended":"Later"`, 1), "context.decision.recommended"},
		{"one option only", `{"decision":{"options":[{"label":"Opt-in"}]}}`, "context.decision.options"},
		{"label with a newline", strings.Replace(valid, `"Opt-in"},`, `"Opt\nin"},`, 1), "context.decision.options[0].label"},
		// Pre-merge review of the Inbox: send accepted these labels, which
		// the board could not answer or told apart.
		{"label with a line separator", strings.Replace(valid, `"Opt-in"},`, `"Opt\u2028in"},`, 1), "context.decision.options[0].label"},
		{"label that starts with @", strings.Replace(valid, `"Opt-in"},`, `"@opt-in"},`, 1), "context.decision.options[0].label"},
		{"label with trailing space", strings.Replace(valid, `"Opt-in"},`, `"Opt-in "},`, 1), "context.decision.options[0].label"},
		{"labels equal but for case", strings.Replace(valid, `"On by default"`, `"opt-in"`, 1), "context.decision.options[1].label"},
		{"blocking null", strings.Replace(valid, `"blocking":false`, `"blocking":null`, 1), "context.decision.blocking"},
		{"valid", valid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runSend([]string{"--root", root, "--me", "lead", "--to", "user", "--kind", "question", "--subject", "guard", "--body", "Pick one.", "--context", tc.context})
			if tc.field == "" {
				if err != nil {
					t.Fatalf("valid decision refused: %v", err)
				}
				return
			}
			if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v (exit %d), want a usage error naming %s", err, GetExitCode(err), tc.field)
			}
		})
	}
}
