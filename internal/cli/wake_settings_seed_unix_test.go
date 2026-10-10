//go:build darwin || linux

package cli

// Purpose: guards the seed retry of an unseeded resume (#1014): the wake
// keeps its argv settings until the guarded seed stores them.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// #1014 round-3 review finding 4a: a held guard must not end the seed retry.
// #1014 round-3 review finding 1: neither must a refused file that appears
// and is removed again; the next tick dropped the argv settings to defaults.
func TestWakeSettingsSeedRetryKeepsArgvSettings(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	canon, err := canonicalizeWakeRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "root": canon, "agent": "a", "generation": "g1"})
	if err := os.WriteFile(filepath.Join(dir, wakeLockFileName), lock, 0o600); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	var insp wakeLockInspection
	_ = agentDir.withFD(func(fd int) error { insp = inspectWakeLockAt(fd, agentDir, root, "a"); return nil })

	flags := defaultWakeSettings()
	flags.holdNormal = 5 * time.Minute
	flags.interrupt = false
	plan, err := planWakeSettingsStartup(nil, false, nil, wakeSettingsObservation{}, flags, []string{"hold_normal", "interrupt"}, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := wakeConfig{
		settings:         plan.settings,
		settingsObserved: observeWakeSettings(nil, false, nil),
		settingsLayers:   plan.layers,
		settingsSource:   wakeSettingsSourceInDir(agentDir, insp, plan.write),
	}
	settingsPath := filepath.Join(dir, wakeSettingsFileName)

	guard, err := os.OpenFile(filepath.Join(dir, wakeLifecycleGuardFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cfg.reloadSettings()
	_ = guard.Close()

	if err := os.WriteFile(settingsPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.reloadSettings()
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	cfg.reloadSettings()

	if cfg.settings != plan.settings {
		t.Fatalf("settings = %+v, want argv %+v", cfg.settings, plan.settings)
	}
	if raw, err := os.ReadFile(settingsPath); err != nil || !bytes.Equal(raw, plan.write) {
		t.Fatalf("stored settings = %q, %v; want seed %q", raw, err, plan.write)
	}
}
