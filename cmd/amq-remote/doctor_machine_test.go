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
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"

	"fiatjaf.com/nostr"
)

// machineFixture is a machine with no endpoint state directory: a Claude
// config dir from the environment, a running machine agent, and a registered
// mailbox binding with a live wake. It returns the settings file and the AMQ root.
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
	savedWake, savedList := doctorWakeCheck, listACPProcesses
	t.Cleanup(func() { doctorWakeCheck, listACPProcesses = savedWake, savedList })
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

// Live probe on Claude Code 2.1.325, bead agent-message-queue-4jxv: a
// second PermissionRequest hook's decision is not applied while AMQ's
// approval hook waits; doctor says so.
func TestDoctorNotesAnotherPermissionRequestHook(t *testing.T) {
	settings, _ := machineFixture(t)
	installHooks(t, true, true)
	saved := runHookProbe
	t.Cleanup(func() { runHookProbe = saved })
	runHookProbe = func(command, gate string) (string, error) {
		return "amq-remote " + version + "\n", nil
	}
	notesOf := func() []boundaryFailure {
		t.Helper()
		out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
		if code != 0 || err != nil {
			t.Fatalf("doctor: code=%d err=%v", code, err)
		}
		notes, _ := out.(map[string]any)["notes"].([]boundaryFailure)
		return notes
	}
	for _, n := range notesOf() {
		if n.Boundary == "hooks" && n.Subject == settings && n.Detail == claude.OtherPermissionHookNote {
			t.Fatal("a foreign-hook note before a foreign hook was present")
		}
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	hooks, _ := file["hooks"].(map[string]any)
	hooks["PermissionRequest"] = append(hooks["PermissionRequest"].([]any), map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "/usr/bin/true"}},
	})
	edited, _ := json.Marshal(file)
	if err := os.WriteFile(settings, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range notesOf() {
		if n.Boundary == "hooks" && n.Subject == settings && n.Detail == claude.OtherPermissionHookNote {
			return
		}
	}
	t.Fatalf("notes = %+v; want the foreign permission-hook note", notesOf())
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
func standaloneMachineAgentFixture(t *testing.T) (map[string]any, string, string, string, string) {
	t.Helper()
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
	return out.(map[string]any), agent, cred.Tag.OwnerPubKey, grantEnd(cred), cred.Relay
}

// The standalone doctor row has the enrolled host and authenticated relay,
// without assuming a record in an empty outbox.
func TestDoctorChecksAStandaloneMachineAgent(t *testing.T) {
	out, agent, owner, grantEnd, _ := standaloneMachineAgentFixture(t)
	row := out["machine_agent"].(machineAgentRow)
	want := machineAgentRow{State: agentRunning, Name: "AMQ Remote @ m", RelayKey: true, Host: hostStandalone, Agent: agent[:8],
		Owner: owner[:8], Relay: "wss://relay.example", RelayState: string(relay.StateAuthenticated), GrantEnd: grantEnd}
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

// An unknown relay write is never forgotten. Doctor reads it without
// owning the outbox and warns once it has been unknown for over a day.
func TestDoctorNotesAnOldUnknownOutboxRecord(t *testing.T) {
	_, agent, _, _, relay := standaloneMachineAgentFixture(t)
	stateDir, err := binding.Path()
	if err != nil {
		t.Fatal(err)
	}
	dir := machineOutboxDir(filepath.Join(filepath.Dir(stateDir), "acp"), buzzop.RelayKey(relay), agent)
	o, err := outbox.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-48 * time.Hour)
	evt := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", "test-channel"}}, CreatedAt: nostr.Timestamp(when.Unix())}
	if err := evt.Sign(nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Prepare("old", "reg", evt, when); err != nil {
		t.Fatal(err)
	}
	if _, err := o.BeginAttempt("old", when); err != nil {
		t.Fatal(err)
	}
	out, code, err := doctor([]string{"--root", canonicalTempDir(t)})
	if code != 0 || err != nil {
		t.Fatalf("doctor = %d, %v", code, err)
	}
	row := out.(map[string]any)["machine_agent"].(machineAgentRow)
	if row.OutboxRecords != 1 || row.OutboxOldestUnknown == "" {
		t.Fatalf("outbox fields = %+v; want one old unknown record", row)
	}
	notes, _ := out.(map[string]any)["notes"].([]boundaryFailure)
	for _, n := range notes {
		if n.Boundary == "machine_agent" && strings.Contains(n.Detail, "no known outcome since") {
			return
		}
	}
	t.Fatalf("notes = %+v; want the old unknown outbox note", notes)
}

// A suspended channel tells the owner why, with no dialog, turn or answer
// sent and a channel reset as the alternative. It is not a failure.
func TestDoctorNotesASuspendedChannel(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session3")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	pub, relayKey := strings.Repeat("e", 64), strings.Repeat("f", 64)
	writeMarker(t, dir, machineMarkerDir, map[string]any{"name": "_machine", "pubkey": pub, "display_name": "AMQ Remote @ box",
		"relay_key": relayKey, "updated": time.Now().UTC().Format(time.RFC3339)})
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "claude", Name: "claude-p-session3"}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(dir, "remote", "acp", "channels", relayKey, pub)
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"name": b.Name, "identity": b.SessionIdentity(), "reg": "r1",
		"state": "active", "suspended": "the channel is archived", "channel_id": "6eff60e4-32ab-48ec-bd3d-f4c97872f370"})
	if err := os.WriteFile(filepath.Join(records, b.Name+".json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	var fails, notes []boundaryFailure
	channels := doctorChannels([]binding.Binding{b}, func(boundary, subject, detail, remedy string) {
		fails = append(fails, boundaryFailure{Boundary: boundary, Subject: subject, Detail: detail, Remedy: remedy})
	}, func(boundary, subject, detail, remedy string) {
		notes = append(notes, boundaryFailure{Boundary: boundary, Subject: subject, Detail: detail, Remedy: remedy})
	})
	if channels[b.Name] != "suspended" || len(fails) != 0 || len(notes) != 1 || !strings.Contains(notes[0].Detail, "the channel is archived") {
		t.Fatalf("channels = %v, failures = %+v, notes = %+v", channels, fails, notes)
	}
}
