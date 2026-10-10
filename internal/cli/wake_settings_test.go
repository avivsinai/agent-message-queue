package cli

// Purpose: guards the startup rule for .wake.settings (#1014): the file owns
// settings. A start without settings flags (repair, coop exec, keepalive,
// env --wake, self-upgrade) runs with the file; a fresh start stores its
// explicit flags in the file; a resume ignores settings flags in argv. It
// also guards the per-key layering of the agent file over the machine file.

import (
	"testing"
	"time"
)

func TestWakeSettingsStartupMerge(t *testing.T) {
	file := []byte(`{"schema":1,"settings":{"hold_normal":"5m","hold_low":"30m"}}`)
	flags := defaultWakeSettings()
	flags.holdNormal = time.Minute
	explicit := []string{"hold_normal"}

	t.Run("file and no flags", func(t *testing.T) {
		plan, err := planWakeSettingsStartup(file, true, nil, wakeSettingsObservation{}, defaultWakeSettings(), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != 5*time.Minute || plan.settings.holdLow != 30*time.Minute {
			t.Fatalf("settings = %+v, want the file holds", plan.settings)
		}
		if plan.write != nil || plan.layers.agentErr != nil {
			t.Fatalf("write = %q, refused = %v; want neither", plan.write, plan.layers.agentErr)
		}
	})

	t.Run("fresh start writes explicit flags", func(t *testing.T) {
		plan, err := planWakeSettingsStartup(file, true, nil, wakeSettingsObservation{}, flags, explicit, false)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != time.Minute || plan.settings.holdLow != 30*time.Minute {
			t.Fatalf("settings = %+v, want the flag over the file", plan.settings)
		}
		stored, err := decodeWakeSettingsDoc(plan.write)
		if err != nil {
			t.Fatalf("decode written file %q: %v", plan.write, err)
		}
		if got, err := stored.effective(); err != nil || got != plan.settings {
			t.Fatalf("written file resolves to %+v (%v), want %+v", got, err, plan.settings)
		}
	})

	t.Run("resume ignores flags", func(t *testing.T) {
		plan, err := planWakeSettingsStartup(file, true, nil, wakeSettingsObservation{}, flags, explicit, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != 5*time.Minute || plan.write != nil {
			t.Fatalf("settings = %+v, write = %q; want the file and no write", plan.settings, plan.write)
		}
	})

	// Review finding (live-apply P1): a resume from an image that kept its
	// settings only in argv found no file and ran on defaults.
	t.Run("resume with no file seeds it from flags", func(t *testing.T) {
		plan, err := planWakeSettingsStartup(nil, false, nil, wakeSettingsObservation{}, flags, explicit, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != time.Minute || plan.write == nil {
			t.Fatalf("settings = %+v, write = %q; want the flags seeded", plan.settings, plan.write)
		}
	})
}

// #1014 machine layer: each key comes from the agent file, else the machine
// file, else the built-in default.
func TestWakeSettingsMachineLayer(t *testing.T) {
	machine, err := decodeWakeSettingsDoc([]byte(`{"schema":1,"settings":{"hold_normal":"5m","hold_low":"30m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := decodeWakeSettingsDoc([]byte(`{"schema":1,"settings":{"hold_normal":"1m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	settings, sources, err := layerWakeSettings(machine, agent)
	if err != nil {
		t.Fatal(err)
	}
	if settings.holdNormal != time.Minute || settings.holdLow != 30*time.Minute || settings.debounce != defaultWakeDebounce {
		t.Fatalf("settings = %+v, want agent hold_normal, machine hold_low, default debounce", settings)
	}
	want := map[string]string{
		"hold_normal": wakeSettingsSourceFile,
		"hold_low":    wakeSettingsSourceMachine,
		"debounce":    wakeSettingsSourceDefault,
	}
	for key, source := range want {
		if sources[key] != source {
			t.Fatalf("source of %s = %q, want %q", key, sources[key], source)
		}
	}
}

// review r1: a cached agent refusal is reused when only the machine file
// changes. The agent file is invalid alone (interrupt is on by default) and
// valid over a machine file that turns interrupt off; once that machine file
// appears, the wake must take the agent file.
func TestWakeSettingsMachineChangeRetriesRefusedAgentFile(t *testing.T) {
	agent := []byte(`{"schema":1,"settings":{"hold_normal":"5m","interrupt_priority":"bogus"}}`)
	var machine []byte
	plan, err := planWakeSettingsStartup(agent, true, nil, observeWakeSettings(nil, false, nil), defaultWakeSettings(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.layers.agentErr == nil {
		t.Fatal("setup: agent file accepted alone, want refused")
	}
	var recorded []wakeSettingsAppliedStatus
	cfg := &wakeConfig{
		settings:                plan.settings,
		settingsObserved:        plan.observed,
		machineSettingsObserved: plan.machineObserved,
		settingsLayers:          plan.layers,
		diagnosticIsTTY:         func() bool { return false },
		settingsSource:          func() ([]byte, bool, error) { return agent, true, nil },
		machineSettingsSource:   func() ([]byte, bool, error) { return machine, machine != nil, nil },
		recordSettingsApplied: func(s wakeSettingsAppliedStatus) error {
			recorded = append(recorded, s)
			return nil
		},
	}
	machine = []byte(`{"schema":1,"settings":{"interrupt":false}}`)
	cfg.reloadSettings()
	if cfg.settings.holdNormal != 5*time.Minute || cfg.settings.interrupt {
		t.Fatalf("settings = %+v, want the agent hold over the machine interrupt=false", cfg.settings)
	}
	if got := recorded[len(recorded)-1]; got.status != wakeSettingsStatusApplied {
		t.Fatalf("applied status = %+v, want applied", got)
	}
}
