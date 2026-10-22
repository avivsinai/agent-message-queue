//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-vd4.3 (surface audit, codex-cli 0.163.0): a
// doorbell at Codex's "Trust this folder?" dialog quit Codex. coop exec now
// writes a Startup marker before exec, and the wake holds until Codex
// records the decision in config.toml, with no hook installed.
func TestCodexTrustDialogHoldsTheDoorbellUntilTrustIsRecorded(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	project := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", []string{"-s", "read-only"})
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatalf("no Startup marker for an untrusted checkout: %v", err)
	}
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)

	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog", h.injected)
	}

	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after trust was recorded = %d, want 1", got)
	}
	// The next launch in the trusted checkout removes the leftover marker.
	writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("Startup marker left after a launch with no dialog: %v", err)
	}
}
