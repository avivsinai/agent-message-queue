//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// A running wake keeps its last read of an unchanged settings file; an edit
// of the agent file or of the machine file still applies on the next tick.
// The test waits out fsq.FileVersionSettle so the first reads are kept.
func TestWakeSettingsEditAppliesAfterAKeptRead(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	machinePath := filepath.Join(home, ".amq", wakeMachineSettingsFileName)
	if err := os.WriteFile(machinePath, []byte(`{"schema":1,"settings":{"interrupt":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "agents", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	writeAgent := func(raw string) {
		t.Helper()
		if err := agentDir.withFD(func(dirfd int) error {
			return writeWakeSettingsFileAt(dirfd, agentDir, []byte(raw))
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent(`{"schema":1,"settings":{"hold_normal":"5m"}}`)
	time.Sleep(fsq.FileVersionSettle + 100*time.Millisecond)

	cfg := &wakeConfig{
		settings:              defaultWakeSettings(),
		diagnosticIsTTY:       func() bool { return false },
		settingsSource:        wakeSettingsSourceInDir(agentDir, wakeLockInspection{}, nil),
		machineSettingsSource: machineWakeSettingsSource(),
		recordSettingsApplied: func(wakeSettingsAppliedStatus) error { return nil },
	}
	if !cfg.reloadSettings() || cfg.settings.holdNormal != 5*time.Minute || cfg.settings.interrupt {
		t.Fatalf("first tick: settings = %+v, want hold 5m and interrupt off", cfg.settings)
	}
	if cfg.reloadSettings() {
		t.Fatal("an unchanged tick reloaded the settings")
	}
	// Both edits settle before the next tick, so only their versions show
	// them.
	writeAgent(`{"schema":1,"settings":{"hold_normal":"7m"}}`)
	if err := os.WriteFile(machinePath, []byte(`{"schema":1,"settings":{"interrupt":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(fsq.FileVersionSettle + 100*time.Millisecond)
	if !cfg.reloadSettings() || cfg.settings.holdNormal != 7*time.Minute || !cfg.settings.interrupt {
		t.Fatalf("after the edits: settings = %+v, want hold 7m and interrupt on", cfg.settings)
	}
}
