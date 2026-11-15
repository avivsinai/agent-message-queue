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

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
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
		if _, err := claude.OptInPermissionHook(home, claude.SettingsPath(home), bin); err != nil {
			t.Fatal(err)
		}
	}
	if stop {
		if _, err := claude.OptInStopHook(home, claude.SettingsPath(home), bin); err != nil {
			t.Fatal(err)
		}
	}
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialogguard.InstallClaudeAt(claude.SettingsPath(home), amq); err != nil {
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
	if len(rows) != 1 || rows[0].Settings != settings || rows[0].Approval != claude.HookPresent ||
		rows[0].Stop != claude.HookPresent || rows[0].Guard != dialogguard.OK || rows[0].Probe != probeOK {
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

// A live wake of a release before dialog entries reads only the single-file
// marker, so doctor warns to restart its session.
func TestDoctorMailboxesWarnsOfAWakeBeforeDialogEntries(t *testing.T) {
	_, root := machineFixture(t)
	doctorWakeCheck = func(string, string) (wakeView, error) {
		return wakeView{LiveWake: true, WakeStatus: "live", ImageStatus: "outdated", RunningVersion: "0.126.1"}, nil
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "claude", Name: "claude-p-session1", Harness: "claude"}
	var failed, noted []string
	doctorMailboxes([]binding.Binding{b},
		func(_, _, detail, _ string) { failed = append(failed, detail) },
		func(_, _, detail, remedy string) { noted = append(noted, detail+"; "+remedy) })
	want := `the wake of handle "claude" runs amq 0.126.1, which cannot read the dialog marker form of 0.128.0 or later; restart that session (exit and resume it)`
	if len(failed) != 0 || len(noted) != 1 || noted[0] != want {
		t.Fatalf("failed = %q, noted = %q; want one note %q", failed, noted, want)
	}
}

// A failing channel prints once, as its FAIL line, not again as an INFO row
// from the channels map (audit 2026-11-10).
// A machine agent that serves inside amq-remote serve is checked from its
// enrolled credential and status file, with no Buzz Desktop check.
func TestDoctorChecksAStandaloneMachineAgent(t *testing.T) {
	machineFixture(t)
	installHooks(t, true, true)
	saved := runHookProbe
	t.Cleanup(func() { runHookProbe = saved })
	runHookProbe = func(string, string) (string, error) { return "amq-remote " + version + "\n", nil }
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	home, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := machinecred.Mint(home)
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	owner[31] = 7
	tag, err := bodykey.SignAuthTag(owner, agent, bodykey.MachineConditions(time.Now().Add(90*24*time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := machinecred.EncodeGrant(agent, "wss://relay.example", *tag)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := machinecred.Enroll(home, grant)
	if err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(filepath.Dir(os.Getenv(binding.EnvPath)), "acp", "agents", machineMarkerDir)
	if err := os.Remove(filepath.Join(markers, strings.Repeat("ab", 32)+".json")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	marker, _ := json.Marshal(map[string]any{"name": "_machine", "pubkey": agent, "display_name": "AMQ Remote @ m", "relay_key": "rk",
		"host": hostStandalone, "worker": self, "updated": now})
	if err := os.WriteFile(filepath.Join(markers, agent+".json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(machineStatus{State: "serving", RelayURL: "wss://relay.example", RelayState: string(relay.StateAuthenticated),
		GrantNotAfter: grantEnd(cred), PID: os.Getpid(), Updated: now})
	if err := os.WriteFile(filepath.Join(home, "remote", "machine", agent, machineStatusFile), status, 0o600); err != nil {
		t.Fatal(err)
	}

	out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
	if err != nil || code != 0 {
		t.Fatalf("doctor: code=%d err=%v failing=%v", code, err, out.(map[string]any)["failing"])
	}
	row := out.(map[string]any)["machine_agent"].(machineAgentRow)
	want := machineAgentRow{State: agentRunning, Name: "AMQ Remote @ m", RelayKey: true, Host: hostStandalone, Agent: agent[:8],
		Owner: tag.OwnerPubKey[:8], Relay: "wss://relay.example", RelayState: string(relay.StateAuthenticated), GrantEnd: grantEnd(cred)}
	if row != want {
		t.Fatalf("machine_agent row = %+v; want %+v", row, want)
	}
}

func TestDoctorTextPrintsAFailingChannelOnce(t *testing.T) {
	var text bytes.Buffer
	printDoctor(&text, doctorReport{
		"failing":  []boundaryFailure{{Boundary: "channel", Subject: "b1", Detail: "the channel of binding b1 is failed"}},
		"channels": map[string]string{"b1": "failed"},
	})
	if strings.Contains(text.String(), "INFO channels") {
		t.Fatalf("doctor text repeats the channel: %q", text.String())
	}
}
