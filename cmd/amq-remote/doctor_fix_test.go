package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
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

// On a fresh home with Claude Code --fix installs the approval hook and the
// wake dialog guard in the settings file of the environment, records the
// owner's opt-in and writes the machine agent's import file; a second run
// changes nothing and keeps the file's mtime.
func TestDoctorFixInstallsEverythingOnceOnAFreshHome(t *testing.T) {
	home := fixSandbox(t)
	settings := claude.SettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	out, _, err := doctor([]string{"--fix"})
	if err != nil {
		t.Fatal(err)
	}
	fixed := out.(map[string]any)["fixed"].([]string)
	if len(fixed) != 3 {
		t.Fatalf("fixed = %q; want the approval hook, the guard and the import file", fixed)
	}
	if state, _ := claude.PermissionHookState(settings); state != claude.HookPresent {
		t.Fatalf("approval hook %q after --fix", state)
	}
	if state, detail := dialogguard.ClaudeAt(settings); state != dialogguard.OK {
		t.Fatalf("guard %q after --fix: %s", state, detail)
	}
	if in, err := claude.ReadHookOptIn(settings); err != nil || in.Approval == nil {
		t.Fatalf("opt-in = %+v, %v", in, err)
	}
	if !fileExists(filepath.Join(home, filepath.FromSlash(machineImportFile))) {
		t.Fatal("no import file")
	}
	before, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = doctor([]string{"--fix"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(settings)
	if fixed := out.(map[string]any)["fixed"].([]string); len(fixed) != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second --fix changed %q (mtime %v -> %v)", fixed, before.ModTime(), after.ModTime())
	}
}

// Review of doctor --fix: on a home without Claude Code it created
// ~/.claude/settings.json, and it installed a login service doctor never
// asked for.
func TestDoctorFixCreatesNothingDoctorDidNotFlag(t *testing.T) {
	home := fixSandbox(t)
	if _, _, err := doctor([]string{"--fix"}); err != nil {
		t.Fatal(err)
	}
	if fileExists(claude.SettingsPath(home)) {
		t.Fatal("--fix created a Claude settings file on a home without Claude Code")
	}
	if path, _ := loginservice.Path(); fileExists(path) {
		t.Fatal("--fix installed a login service doctor did not advise")
	}
}

// Review of doctor --fix: it installed again the approval hook the owner
// had removed with uninstall-approval-hook.
func TestDoctorFixKeepsAHookTheOwnerRemoved(t *testing.T) {
	home := fixSandbox(t)
	settings := claude.SettingsPath(home)
	bin, _, err := stableExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claude.OptInPermissionHook(home, settings, bin); err != nil {
		t.Fatal(err)
	}
	if err := claude.OptOutHook(settings, claude.HookApproval); err != nil {
		t.Fatal(err)
	}
	out, _, err := doctor([]string{"--fix"})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := claude.PermissionHookState(settings); state != claude.HookMissing {
		t.Fatalf("approval hook %q after --fix; want it left removed", state)
	}
	for _, f := range out.(map[string]any)["failing"].([]boundaryFailure) {
		if f.Boundary == "hooks" && strings.Contains(f.Detail, "approval hook") {
			t.Fatalf("doctor fails the hook the owner removed: %+v", f)
		}
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

// An approval hook of the older form (--wait 600, timeout 630) becomes the
// current form, with the one-day timeout, after doctor --fix.
func TestDoctorFixUpgradesTheTenMinuteApprovalHook(t *testing.T) {
	home := fixSandbox(t)
	settings := claude.SettingsPath(home)
	bin, _, err := stableExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claude.OptInPermissionHook(home, settings, bin); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(bytes.Replace(raw, []byte(`claude permission-hook"`), []byte(`claude permission-hook --wait 600"`), 1), []byte(`"timeout":86400`), []byte(`"timeout":630`), 1)
	if err := os.WriteFile(settings, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if form, _ := claude.PermissionHookForm(home, settings); form != claude.HookFormStale {
		t.Fatalf("old form reads as %q; want stale", form)
	}
	if _, _, err := doctor([]string{"--fix"}); err != nil {
		t.Fatal(err)
	}
	if form, _ := claude.PermissionHookForm(home, settings); form != claude.HookFormCurrent {
		t.Fatalf("form after --fix = %q; want current", form)
	}
	if raw, _ := os.ReadFile(settings); !bytes.Contains(raw, []byte(`"timeout":86400`)) {
		t.Fatalf("settings after --fix = %s; want the one-day timeout", raw)
	}
}
