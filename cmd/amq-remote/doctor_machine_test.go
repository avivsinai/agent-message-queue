package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// machineFixture is a machine with no endpoint state directory: a Claude
// config dir from the environment, a running machine agent that Buzz
// Desktop starts on launch, and a registered mailbox binding with a live
// wake. It returns the settings file and the AMQ root.
func machineFixture(t *testing.T) (settings, root string) {
	t.Helper()
	dir := canonicalTempDir(t)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("PATH", filepath.Join(dir, "empty")) // no claude to ask for its version
	config := filepath.Join(dir, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root = filepath.Join(dir, "p", ".agent-mail", "session1")
	for _, d := range []string{config, filepath.Join(root, "meta"), filepath.Join(dir, "remote", "acp", "agents", "_machine")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "meta", "config.json"), `{"version":1,"created_utc":"2026-11-06T00:00:00Z","agents":["claude"]}`+"\n")
	key := strings.Repeat("ab", 32)
	marker, _ := json.Marshal(map[string]any{"name": "machine", "display_name": "AMQ Remote @ m", "relay_key": "rk",
		"updated": time.Now().UTC().Format(time.RFC3339)})
	write(filepath.Join(dir, "remote", "acp", "agents", "_machine", key+".json"), string(marker))
	managed := filepath.Join(dir, "managed-agents.json")
	write(managed, `[{"pubkey":"`+key+`","start_on_app_launch":true,"name":"machine"}]`)
	savedPath, savedWake, savedList := buzzManagedAgentsPath, doctorWakeCheck, listACPProcesses
	t.Cleanup(func() { buzzManagedAgentsPath, doctorWakeCheck, listACPProcesses = savedPath, savedWake, savedList })
	buzzManagedAgentsPath = func() (string, error) { return managed, nil }
	listACPProcesses = func(context.Context) ([]acpProcess, error) { return nil, nil }
	doctorWakeCheck = func(string, string) (wakeView, error) {
		return wakeView{LiveWake: true, WakeStatus: "live", ImageStatus: "current"}, nil
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "claude", Name: "claude-p-session1", Harness: "claude"}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(config, "settings.json"), root
}

// installHooks installs the approval hook, the Stop hook and the wake dialog
// guard in the settings file of the environment, as their installers do.
func installHooks(t *testing.T, approval, stop bool) {
	t.Helper()
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "amq-remote")
	if approval {
		if err := claude.InstallPermissionHook(home, bin, claude.DefaultPermissionWait); err != nil {
			t.Fatal(err)
		}
	}
	if stop {
		if err := claude.InstallStopHook(home, bin); err != nil {
			t.Fatal(err)
		}
	}
	if err := amqcli.Run([]string{"wake", "install-dialog-hook", "--harness", "claude"}, "test"); err != nil {
		t.Fatal(err)
	}
}

// The hook pass runs with no endpoint and finds the approval hook, the Stop
// hook and the guard in the settings file, and the installed approval
// command reaches amq-remote with the binding of that file's session as the
// gate.
func TestDoctorHookPassFindsEveryHook(t *testing.T) {
	settings, _ := machineFixture(t)
	installHooks(t, true, true)
	var ran, gate string
	saved := runHookProbe
	t.Cleanup(func() { runHookProbe = saved })
	runHookProbe = func(command, g string) (string, error) {
		ran, gate = command, g
		return "amq-remote " + version + "\n", nil
	}
	out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
	if err != nil || code != 0 {
		t.Fatalf("doctor: code=%d err=%v failing=%v", code, err, out.(map[string]any)["failing"])
	}
	rows := out.(map[string]any)["hooks"].([]hookFile)
	if len(rows) != 1 || rows[0].Settings != settings || rows[0].Approval != claude.StopHookPresent ||
		rows[0].Stop != claude.StopHookPresent || rows[0].Guard != amqcli.DialogGuardOK || rows[0].Probe != probeOK {
		t.Fatalf("hook rows = %+v", rows)
	}
	want, _ := claude.PermissionHookCommand(settings)
	file, _ := binding.FilePath("claude-p-session1")
	if ran != want || gate != file {
		t.Fatalf("probe ran %q with gate %q; want %q with %q", ran, gate, want, file)
	}
}

// Observed 2026-11-05: an approval hook removed from settings.json left
// every approval of a mailbox session in the terminal for 8 hours, and
// `amq-remote doctor` returned before any hook check because no endpoint
// state directory existed, so it named nothing.
func TestDoctorReportsAMissingApprovalHookWithoutAnEndpoint(t *testing.T) {
	settings, _ := machineFixture(t)
	installHooks(t, false, false)
	out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	failing := out.(map[string]any)["failing"].([]boundaryFailure)
	if code != protocol.ExitActionRequired || len(failing) != 1 || failing[0].Boundary != "hooks" || failing[0].Subject != settings {
		t.Fatalf("exit=%d failing=%+v; want one hooks failure for %s", code, failing, settings)
	}
	// Only native mode needs the Stop hook: on this mailbox-only machine
	// its absence is a note.
	stopNoted := false
	for _, n := range out.(map[string]any)["notes"].([]boundaryFailure) {
		stopNoted = stopNoted || n.Boundary == "hooks" && n.Subject == settings && strings.Contains(n.Detail, "Stop hook")
	}
	if !stopNoted {
		t.Fatalf("missing Stop hook not noted: %v", out.(map[string]any)["notes"])
	}
	var text bytes.Buffer
	printDoctor(&text, doctorReport(out.(map[string]any)))
	if line := strings.SplitN(text.String(), "\n", 2)[0]; !strings.HasPrefix(line, "FAIL hooks "+settings+": ") || !strings.Contains(line, "; fix: run `amq-remote claude install-approval-hook`") {
		t.Fatalf("doctor text = %q", text.String())
	}
}

// Observed 2026-11-05: /amq-remote attached a Claude session whose settings
// had lost the approval hook and gave no signal; the skill learned it only
// from a doctor run that returned early.
func TestAttachReportsAMissingApprovalHook(t *testing.T) {
	_, root := machineFixture(t)
	saved := detectHarness
	t.Cleanup(func() { detectHarness = saved })
	detectHarness = func(*codexView, string, string) harnessSession { return harnessSession{Kind: "claude"} }
	pinMailboxSession(t, root)
	var out bytes.Buffer
	if code, err := attach([]string{"--self", "--json"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("attach: code=%d err=%v", code, err)
	}
	var got struct {
		ApprovalHook string `json:"approval_hook"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.ApprovalHook != approvalHookMissing {
		t.Fatalf("attach output %s: approval_hook %q; want missing (%v)", out.String(), got.ApprovalHook, err)
	}
	if b, err := binding.ReadNamed("claude-p-session1"); err != nil || b.ConfigDir != os.Getenv("CLAUDE_CONFIG_DIR") {
		t.Fatalf("binding config_dir = %q (%v); want %s", b.ConfigDir, err, os.Getenv("CLAUDE_CONFIG_DIR"))
	}
}
