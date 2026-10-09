//go:build darwin || linux

package cli

// Purpose: guards amq wake config (#1014): set, show and unset round trip on
// one root, and a bad value is refused before anything is written.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	for _, args := range [][]string{
		{"--hold-low", "banana"},
	} {
		_, err := captureStdout(t, func() error { return runWakeConfig(append(append([]string{}, base...), args...)) })
		if GetExitCode(err) != ExitUsage {
			t.Fatalf("wake config %v exit = %d (%v), want usage", args, GetExitCode(err), err)
		}
		after, readErr := os.ReadFile(settingsPath)
		if readErr != nil || string(after) != string(before) {
			t.Fatalf("wake config %v changed the settings file: %q (%v)", args, after, readErr)
		}
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
