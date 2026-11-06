package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
)

// fixSandbox is a fresh home with the amq-remote and amq binaries on PATH
// and no claude, as `amq-remote doctor --fix` sees a new machine.
func fixSandbox(t *testing.T) (home string) {
	t.Helper()
	serviceSandbox(t)
	home, _ = os.UserHomeDir()
	self, _ := serviceExecutable()
	if err := os.WriteFile(filepath.Join(filepath.Dir(self), "amq"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(self))
	saved := runMachineSetup
	t.Cleanup(func() { runMachineSetup = saved })
	runMachineSetup = func(out string) error { return os.WriteFile(out, []byte("{}\n"), 0o600) }
	return home
}

// On a fresh home --fix installs the approval hook and the wake dialog
// guard in the settings file of the environment, records the owner's
// opt-in, writes the machine agent's import file and installs the login
// service; a second run changes nothing and keeps the file's mtime.
func TestDoctorFixInstallsEverythingOnceOnAFreshHome(t *testing.T) {
	home := fixSandbox(t)
	settings := claude.SettingsPath(home)
	out, _, err := doctor([]string{"--fix"})
	if err != nil {
		t.Fatal(err)
	}
	fixed := out.(map[string]any)["fixed"].([]string)
	if len(fixed) != 4 {
		t.Fatalf("fixed = %q; want the approval hook, the guard, the import file and the service", fixed)
	}
	if state, _ := claude.PermissionHookState(settings); state != claude.StopHookPresent {
		t.Fatalf("approval hook %q after --fix", state)
	}
	if state, detail := amqcli.ClaudeDialogGuardAt(settings); state != amqcli.DialogGuardOK {
		t.Fatalf("guard %q after --fix: %s", state, detail)
	}
	if in, err := claude.ReadHookOptIn(settings); err != nil || in.Approval == nil {
		t.Fatalf("opt-in = %+v, %v", in, err)
	}
	if !fileExists(filepath.Join(home, "Downloads", machineImportName)) {
		t.Fatal("no import file")
	}
	if path, _ := loginservice.Path(); !fileExists(path) {
		t.Fatal("no login service")
	}
	before, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	// The installed service starts no endpoint here, so the second run
	// still fails the endpoint; it repairs nothing more.
	out, _, err = doctor([]string{"--fix"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(settings)
	if fixed := out.(map[string]any)["fixed"].([]string); len(fixed) != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second --fix changed %q (mtime %v -> %v)", fixed, before.ModTime(), after.ModTime())
	}
}

// Review of the doctor that checks every mode: a machine with only
// ~/.claude, no binding and no machine agent, failed the missing approval
// hook and guard, although their gate stays closed there by design.
func TestDoctorNotesHooksWhileNothingCanUseThem(t *testing.T) {
	home := canonicalTempDir(t)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
	if err != nil || code != 0 {
		t.Fatalf("doctor: code=%d err=%v failing=%+v", code, err, out.(map[string]any)["failing"])
	}
}

// Review of the doctor that checks every mode: the config_dir of a binding
// whose session ended stayed in the hook pass, so its file failed for ever.
func TestDoctorSkipsTheConfigDirOfAnEndedSession(t *testing.T) {
	settings, _ := machineFixture(t)
	installHooks(t, true, true)
	dead := filepath.Join(canonicalTempDir(t), "claude-dead")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: filepath.Join(dead, "gone"), Handle: "old", Name: "old-session",
		Harness: "claude", ConfigDir: dead}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	out, _, err := doctor([]string{"--root", canonicalTempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range out.(map[string]any)["hooks"].([]hookFile) {
		if row.Settings != settings {
			t.Fatalf("hook pass checked %s of an ended session", row.Settings)
		}
	}
}

// Review of the silent probe: AMQ_HOOK_PROBE=1 leaked into a session's
// environment made the receiver print its version for a real dialog, so
// the dialog never reached Buzz.
func TestApprovalProbeNeedsAnEmptyStdin(t *testing.T) {
	t.Setenv(hookProbeEnv, "1")
	var out bytes.Buffer
	if code := permissionHook(nil, strings.NewReader("not a dialog payload"), &out); code != 0 || strings.Contains(out.String(), "amq-remote") {
		t.Fatalf("probe with a payload: code=%d out=%q", code, out.String())
	}
	out.Reset()
	if permissionHook(nil, strings.NewReader(""), &out); !strings.HasPrefix(out.String(), "amq-remote ") {
		t.Fatalf("probe = %q", out.String())
	}
}
