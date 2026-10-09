package cli

// Purpose: guards the startup rule for .wake.settings (#1014): the file owns
// settings. A start without settings flags (repair, coop exec, keepalive,
// env --wake, self-upgrade) runs with the file; a fresh start stores its
// explicit flags in the file; a resume ignores settings flags in argv.

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
		plan, err := planWakeSettingsStartup(file, true, nil, defaultWakeSettings(), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != 5*time.Minute || plan.settings.holdLow != 30*time.Minute {
			t.Fatalf("settings = %+v, want the file holds", plan.settings)
		}
		if plan.write != nil || plan.refused != nil {
			t.Fatalf("write = %q, refused = %v; want neither", plan.write, plan.refused)
		}
	})

	t.Run("fresh start writes explicit flags", func(t *testing.T) {
		plan, err := planWakeSettingsStartup(file, true, nil, flags, explicit, false)
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
		plan, err := planWakeSettingsStartup(file, true, nil, flags, explicit, true)
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
		plan, err := planWakeSettingsStartup(nil, false, nil, flags, explicit, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.settings.holdNormal != time.Minute || plan.write == nil {
			t.Fatalf("settings = %+v, write = %q; want the flags seeded", plan.settings, plan.write)
		}
	})
}
