//go:build darwin || linux

package cli

// Purpose: guards amq wake config (#1014): set, show and unset round trip on
// one root, and a bad value is refused before anything is written.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeConfigRoundTrip(t *testing.T) {
	root := t.TempDir()
	base := []string{"--root", root, "--me", "claude", "--json"}
	if err := os.MkdirAll(fsq.AgentBase(root, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(fsq.AgentBase(root, "claude"), wakeSettingsFileName)

	show := func(extra ...string) wakeConfigJSON {
		t.Helper()
		out, err := captureStdout(t, func() error { return runWakeConfig(append(append([]string{}, base...), extra...)) })
		if err != nil {
			t.Fatalf("wake config %v: %v", extra, err)
		}
		var got wakeConfigJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		return got
	}

	got := show()
	if s := got.Settings["hold_normal"]; s.Value != "0s" || s.Source != "default" {
		t.Fatalf("fresh hold_normal = %+v, want 0s from default", s)
	}
	if got.Wake.Status != wakeSettingsRunNone {
		t.Fatalf("wake status = %q, want %q", got.Wake.Status, wakeSettingsRunNone)
	}

	got = show("--hold-normal", "5m")
	if s := got.Settings["hold_normal"]; s.Value != "5m0s" || s.Source != "file" {
		t.Fatalf("after set hold_normal = %+v, want 5m0s from file", s)
	}
	if got := show(); got.Settings["hold_normal"].Value != "5m0s" {
		t.Fatalf("show after set = %+v, want the stored value", got.Settings["hold_normal"])
	}

	got = show("--unset", "hold_normal")
	if s := got.Settings["hold_normal"]; s.Value != "0s" || s.Source != "default" {
		t.Fatalf("after unset hold_normal = %+v, want 0s from default", s)
	}

	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = captureStdout(t, func() error { return runWakeConfig(append(append([]string{}, base...), "--hold-low", "banana")) })
	if GetExitCode(err) != ExitUsage {
		t.Fatalf("wake config --hold-low banana exit = %d (%v), want usage", GetExitCode(err), err)
	}
	after, readErr := os.ReadFile(settingsPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("wake config --hold-low banana changed the settings file: %q (%v)", after, readErr)
	}

	// --reset replaces a refused file with exactly the given keys.
	if err := os.WriteFile(settingsPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := show(); got.File.Status != "refused" {
		t.Fatalf("garbage file status = %q, want refused", got.File.Status)
	}
	got = show("--reset", "--hold-low", "1m")
	if s := got.Settings["hold_low"]; s.Value != "1m0s" || s.Source != "file" || got.File.Status != "ok" {
		t.Fatalf("after reset hold_low = %+v, file %+v, want 1m0s from file, ok", s, got.File)
	}

	// Regression (#1014 live E2E S8): a typo handle exited 1 because the wake
	// diagnostic wrap hid the not-found code.
	_, err = captureStdout(t, func() error { return runWakeConfig([]string{"--root", root, "--me", "nosuch"}) })
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("wake config --me nosuch exit = %d (%v), want not found", GetExitCode(err), err)
	}
	if _, statErr := os.Lstat(fsq.AgentBase(root, "nosuch")); !os.IsNotExist(statErr) {
		t.Fatalf("wake config --me nosuch created the mailbox: %v", statErr)
	}
}

// #1014 round-3 review findings 2 and 4b: with a valid wake lock, no status
// sidecar and an absent file, set exits 6 only after the startup grace and
// writes nothing.
func TestWakeConfigSetRefusesUnreportedWakeAfterGrace(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "1783327533.465308000",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "gen-1",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "start-1",
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "1783327533.407566000",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	old := wakeConfigUnreportedGrace
	wakeConfigUnreportedGrace = 50 * time.Millisecond
	t.Cleanup(func() { wakeConfigUnreportedGrace = old })

	start := time.Now()
	_, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "codex", "--hold-low", "10m"})
	})
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("set on unreported wake exit = %d (%v), want action required", GetExitCode(err), err)
	}
	if time.Since(start) < wakeConfigUnreportedGrace {
		t.Fatalf("set refused after %s, before the %s grace", time.Since(start), wakeConfigUnreportedGrace)
	}
	if _, statErr := os.Lstat(filepath.Join(fsq.AgentBase(root, "codex"), wakeSettingsFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("refused set wrote the settings file: %v", statErr)
	}
}
