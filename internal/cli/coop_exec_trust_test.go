//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
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
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(codexHome, "config.toml")
	// #1180 review: a directory saved as untrusted still opens on Codex's
	// restricted trust dialog, so it gets the marker too.
	untrusted := "[projects.\"" + real + "\"]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
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

	// #1180 review: a half-written config.toml proves nothing, so the hold
	// stays.
	if err := os.WriteFile(config, []byte("[projects.\""), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q after the trust file became unreadable", h.injected)
	}

	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after trust was recorded = %d, want 1", got)
	}
	// The next launch in the trusted checkout removes the leftover marker.
	clearStaleStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("Startup marker left after a launch with no dialog: %v", err)
	}

	// #1180 ruling: a directory that stays untrusted has no readiness
	// signal without the Codex SessionStart hook; after 10 minutes the
	// existing aging applies: input still held, attention output instead.
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	rang := len(h.injected)
	h.send("b", format.PriorityUrgent)
	h.advance(wakeDialogQuietHold)
	h.scan(h.cfg)
	if len(h.injected) != rang || h.cfg.dialogHold != wakeDialogUnknown {
		t.Fatalf("aged untrusted hold: injected %q, dialog state %v; want held with attention", h.injected[rang:], h.cfg.dialogHold)
	}
}

// #1180 review: coop exec wrote or removed the Startup marker before
// prepareCoopWakeLock refused a launch for another live owner, so a refused
// launch from a trusted directory cleared the running owner's trust hold.
func TestRefusedCoopLaunchKeepsTheLiveOwnersStartupMarker(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	project := secureTempDirForTest(t)
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trusted := `{"projects":{"` + real + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := secureTempDirForTest(t)
	// The test process is running and is no amq wake: a live owner coop
	// exec must not touch.
	writeWakeLockExactForTest(t, root, "claude", wakeLock{PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "claude"})
	marker := filepath.Join(fsq.AgentBase(root, "claude"), wakeDialogFileName)
	owners := []byte(`{"schema":1,"event":"Startup","harness":"claude","cwd":"/elsewhere","opened_at":"x"}` + "\n")
	if err := os.WriteFile(marker, owners, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stubCoopExecSentinel(t)

	if err := runCoopExec([]string{"--root", root, "--me", "claude", "-y", "claude"}); err == nil {
		t.Fatal("coop exec started beside a live wake owner")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(owners) {
		t.Fatalf("live owner's Startup marker = %q, %v; want it unchanged", got, err)
	}
}

// #1180 review: coop exec wrote the Startup marker only after its wake was
// ready, so a message that arrived in between was typed into the terminal,
// waited there, and answered the trust dialog when the harness started.
// The marker now exists before the wake process starts.
func TestStartupMarkerExistsBeforeTheWakeStarts(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(secureTempDirForTest(t))
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := secureTempDirForTest(t)
	marker := filepath.Join(fsq.AgentBase(root, "claude"), wakeDialogFileName)
	atStart := errors.New("wake never started")
	old := startCoopWake
	startCoopWake = func(*exec.Cmd) error {
		_, atStart = os.Lstat(marker)
		return errors.New("stub: no wake")
	}
	t.Cleanup(func() { startCoopWake = old })
	sentinel := stubCoopExecSentinel(t)

	if err := runCoopExec([]string{"--root", root, "--me", "claude", "claude"}); !errors.Is(err, sentinel) {
		t.Fatalf("coop exec = %v, want the exec sentinel", err)
	}
	if atStart != nil {
		t.Fatalf("Startup marker when the wake started: %v", atStart)
	}
}
