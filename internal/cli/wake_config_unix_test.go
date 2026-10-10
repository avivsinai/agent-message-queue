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

	// --machine sets the layer below the agent file; --me only
	// adds that agent's wake block.
	machine := func(extra ...string) wakeConfigJSON {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return runWakeConfig(append([]string{"--machine", "--root", root, "--me", "claude", "--json"}, extra...))
		})
		if err != nil {
			t.Fatalf("wake config --machine %v: %v", extra, err)
		}
		var got wakeConfigJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		return got
	}
	got = machine("--hold-normal", "10m")
	if s := got.Settings["hold_normal"]; s.Value != "10m0s" || s.Source != "machine" || got.MachineFile.Status != "ok" || got.Wake == nil {
		t.Fatalf("machine set hold_normal = %+v, machine_file %+v, wake %+v, want 10m0s from machine, ok, a wake block", s, got.MachineFile, got.Wake)
	}
	if s := show().Settings["hold_normal"]; s.Value != "10m0s" || s.Source != "machine" {
		t.Fatalf("agent show hold_normal = %+v, want 10m0s from machine", s)
	}
	if s := show("--hold-normal", "1m").Settings["hold_normal"]; s.Value != "1m0s" || s.Source != "file" {
		t.Fatalf("agent set over machine hold_normal = %+v, want 1m0s from file", s)
	}
	if s := show("--unset", "hold_normal").Settings["hold_normal"]; s.Value != "10m0s" || s.Source != "machine" {
		t.Fatalf("agent unset hold_normal = %+v, want 10m0s from machine", s)
	}
	if s := machine("--unset", "hold_normal").Settings["hold_normal"]; s.Value != "0s" || s.Source != "default" {
		t.Fatalf("machine unset hold_normal = %+v, want 0s from default", s)
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

// machineConfigWakeFixture starts a valid wake lock for agent "codex" and
// returns its root. Helper for the machine-layer review r1 rows below.
func machineConfigWakeFixture(t *testing.T) string {
	t.Helper()
	const wakePID = 4243
	t.Setenv("HOME", secureTempDirForTest(t))
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
	return root
}

func writeWakeAppliedForTest(t *testing.T, root string, applied wakeSettingsAppliedFile) {
	t.Helper()
	applied.Schema = wakeSettingsSchemaV1
	applied.Root = canonicalWakeRoot(root)
	applied.Agent = "codex"
	applied.Generation = "gen-1"
	data, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fsq.AgentBase(root, "codex"), wakeSettingsAppliedFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// review r1: a machine file made set refuse a running 0.94.0 wake that has no
// agent file. Its sidecar has no machine fields and matches the absent agent
// digest, so the unseeded-argv gate must let the set through.
func TestWakeConfigSetIgnoresMachineFileForUnseededGate(t *testing.T) {
	root := machineConfigWakeFixture(t)
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--machine", "--root", root, "--me", "codex", "--hold-normal", "1m"})
	}); err != nil {
		t.Fatalf("machine set: %v", err)
	}
	writeWakeAppliedForTest(t, root, wakeSettingsAppliedFile{
		Status: wakeSettingsStatusApplied,
		Digest: wakeSettingsDigest(nil, false),
	})
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "codex", "--hold-low", "10m"})
	}); err != nil {
		t.Fatalf("agent set on a 0.94.0 wake with a machine file: %v", err)
	}
}

// review r1: an agent-only --wait change exited 1 although the wake applied
// it, whenever the machine file was refused.
func TestWakeConfigAgentWaitIgnoresRefusedMachineFile(t *testing.T) {
	root := machineConfigWakeFixture(t)
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "codex", "--reset", "--hold-low", "10m"})
	}); err != nil {
		t.Fatalf("seed agent file: %v", err)
	}
	agentRaw, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, "codex"), wakeSettingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	machinePath, err := machineWakeSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(machinePath), 0o700); err != nil {
		t.Fatal(err)
	}
	machineRaw := []byte("garbage")
	if err := os.WriteFile(machinePath, machineRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	writeWakeAppliedForTest(t, root, wakeSettingsAppliedFile{
		Status:        wakeSettingsStatusApplied,
		Digest:        wakeSettingsDigest(agentRaw, true),
		MachineStatus: wakeConfigMachineRefused,
		MachineDigest: wakeSettingsDigest(machineRaw, true),
		MachineError:  "bad",
	})
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "codex", "--wait", "--timeout", "5s"})
	}); err != nil {
		t.Fatalf("agent --wait with a refused machine file: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "codex", "--machine", "--wait", "--timeout", "5s"})
	}); err == nil {
		t.Fatal("--machine --wait with a refused machine file exited 0, want an error")
	}
}

// review r2: --machine --wait still exits 1 on a refused agent file although
// the wake applied the machine change.
func TestWakeConfigMachineWaitIgnoresRefusedAgentFile(t *testing.T) {
	root := machineConfigWakeFixture(t)
	agentPath := filepath.Join(fsq.AgentBase(root, "codex"), wakeSettingsFileName)
	agentRaw := []byte("garbage")
	if err := os.WriteFile(agentPath, agentRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--machine", "--root", root, "--me", "codex", "--hold-normal", "1m"})
	}); err != nil {
		t.Fatalf("machine set: %v", err)
	}
	machinePath, err := machineWakeSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	machineRaw, err := os.ReadFile(machinePath)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeAppliedForTest(t, root, wakeSettingsAppliedFile{
		Status:        wakeSettingsStatusRefused,
		Digest:        wakeSettingsDigest(agentRaw, true),
		Error:         "decode wake settings: bad",
		MachineStatus: wakeSettingsStatusApplied,
		MachineDigest: wakeSettingsDigest(machineRaw, true),
	})
	if _, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--machine", "--root", root, "--me", "codex", "--wait", "--timeout", "5s"})
	}); err != nil {
		t.Fatalf("--machine --wait with a refused agent file: %v", err)
	}
}

// review r1: with a group-writable ~/.amq and no wake.settings, every wake
// reported the machine layer as refused for a file nobody made.
func TestReadMachineWakeSettingsGroupWritableHomeNoFile(t *testing.T) {
	home := secureTempDirForTest(t)
	t.Setenv("HOME", home)
	amqDir := filepath.Join(home, ".amq")
	if err := os.Mkdir(amqDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(amqDir, 0o775); err != nil {
		t.Fatal(err)
	}
	raw, exists, err := readMachineWakeSettings()
	if err != nil || exists || raw != nil {
		t.Fatalf("readMachineWakeSettings = (%q, %v, %v), want no file and no error", raw, exists, err)
	}
}

// review r1: --machine --me reported the machine file ok while the agent's
// wake refused it, because only plain --me layered the two files.
func TestWakeConfigMachineWithMeReportsLayeredRefusal(t *testing.T) {
	t.Setenv("HOME", secureTempDirForTest(t))
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) wakeConfigJSON {
		t.Helper()
		args = append([]string{"--root", root, "--me", "codex"}, args...)
		out, err := captureStdout(t, func() error { return runWakeConfig(append(args, "--json")) })
		if err != nil {
			t.Fatalf("wake config %v: %v", args, err)
		}
		var got wakeConfigJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		return got
	}
	run("--machine", "--interrupt=false", "--interrupt-label", "")
	run("--interrupt")
	plain := run()
	if plain.MachineFile.Status != "refused" {
		t.Fatalf("fixture: plain --me machine_file = %q, want refused", plain.MachineFile.Status)
	}
	got := run("--machine")
	if got.MachineFile.Status != "refused" {
		t.Fatalf("--machine --me machine_file = %q, want refused as plain --me shows", got.MachineFile.Status)
	}
}
