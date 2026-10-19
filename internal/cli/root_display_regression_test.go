package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Review finding from PR #252's discovery regression: known bases must not be
// reclassified by display hints, including before their first session exists.
func TestDisplayClassificationPreservesKnownBase(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	base := filepath.Join(home, defaultCoopRoot)
	for _, dir := range []string{base, filepath.Join(home, ".claude", "agents")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := classifyRootForDisplay(base); got != base {
		t.Errorf("display base = %q, want known base %q", got, base)
	}
	if got := resolveSessionNameForDisplay(base); got != "" {
		t.Errorf("base has display session %q, want none", got)
	}
	text, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", base})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "Base root: "+base+"\n") {
		t.Errorf("empty known base must remain the discovery base, got:\n%s", text)
	}
}

// Audit finding A1: removing display hints loses unconfigured custom sessions
// when their valid agent handle "agents" resembles a nested session directory.
func TestRunWhoPreservesUnknownSessionWithAgentsHandle(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	base := filepath.Join(home, "mail")
	root := filepath.Join(base, "collab")
	if err := fsq.EnsureAgentDirs(root, "agents"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(filepath.Join(base, "qa"), "codex"); err != nil {
		t.Fatal(err)
	}
	output, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(output), &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "collab" || sessions[1].Name != "qa" {
		t.Errorf("custom session must discover collab and qa, got %s", output)
	}
}
