package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// Regression: #1080, unrelated Claude/Cursor agents directories
// made who scan the base's parent and report no sessions. Classifier-only tests
// missed the display heuristic used by the command.
func TestRunWhoPreservesBaseBesideUnrelatedAgents(t *testing.T) {
	for _, test := range []struct {
		name    string
		base    string
		sibling string
		source  string
	}{
		{name: "default", base: defaultCoopRoot, sibling: ".claude"},
		{name: "environment", base: "mail", sibling: ".cursor", source: "env"},
		{name: "configuration", base: "mail", sibling: ".claude", source: "config"},
		// #1080: an unconfigured custom base beside ~/.claude/agents
		// still showed HOME as the base and no sessions.
		{name: "custom", base: "mail", sibling: ".claude"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(home)
			base := filepath.Join(home, test.base)
			root := filepath.Join(base, "collab")
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			if err := presence.Write(root, presence.New("codex", "idle", "", time.Now())); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, test.sibling, "agents"), 0o700); err != nil {
				t.Fatal(err)
			}
			switch test.source {
			case "env":
				t.Setenv(envBaseRoot, base)
			case "config":
				data, err := json.Marshal(map[string]string{"root": base})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".amqrc"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			output, err := captureEnvStdout(t, func() error {
				return runWho([]string{"--root", base, "--json"})
			})
			if err != nil {
				t.Fatalf("runWho JSON: %v", err)
			}
			var sessions []struct {
				Name   string `json:"name"`
				Agents []struct {
					Handle string `json:"handle"`
					Active bool   `json:"active"`
				} `json:"agents"`
			}
			if err := json.Unmarshal([]byte(output), &sessions); err != nil {
				t.Fatalf("decode who JSON: %v (output: %s)", err, output)
			}
			if len(sessions) != 1 || sessions[0].Name != "collab" || len(sessions[0].Agents) != 1 ||
				sessions[0].Agents[0].Handle != "codex" || !sessions[0].Agents[0].Active {
				t.Errorf("who must discover collab's active codex under %s, got %s", base, output)
			}

			text, err := captureEnvStdout(t, func() error {
				return runWho([]string{"--root", base})
			})
			if err != nil {
				t.Fatalf("runWho text: %v", err)
			}
			if !strings.Contains(text, "Base root: "+base+"\n") || !strings.Contains(text, "codex  active") {
				t.Errorf("who must display the requested base and active codex, got:\n%s", text)
			}
		})
	}
}
