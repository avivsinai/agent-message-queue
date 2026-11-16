package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
	"github.com/avivsinai/agent-message-queue/internal/roster"
	"github.com/avivsinai/agent-message-queue/internal/route"
)

// The machine pass of doctor checks every link that does not need the
// endpoint, so it runs with or without a state directory: the Claude hooks
// in every settings file the machine's sessions read, the machine agent,
// the channel of each binding, and the handle and wake of each mailbox
// binding.

type reportFunc = func(boundary, subject, detail, remedy string)

// doctorMachine runs the machine pass over the bindings all and the
// manifest under stateDir, and records its rows in report.
func doctorMachine(stateDir string, all []binding.Binding, report map[string]any, fail, note reportFunc) {
	machine := machineOf(time.Now())
	report["hooks"] = doctorHooks(claudeSettingsFiles(stateDir, all), all, nativeConfigured(stateDir, all), machine, fail, note)
	report["machine_agent"] = doctorMachineAgent(machine, fail, note)
	if channels := doctorChannels(all, fail, note); len(channels) > 0 {
		report["channels"] = channels
	}
	if mailboxes := doctorMailboxes(all, fail, note); len(mailboxes) > 0 {
		report["mailboxes"] = mailboxes
	}
}

// nativeConfigured reports whether this machine uses native mode: a native
// binding, or an endpoint manifest with an entry. Only native mode needs the
// Stop hook.
func nativeConfigured(stateDir string, all []binding.Binding) bool {
	for _, b := range all {
		if !b.Mailbox() {
			return true
		}
	}
	mf, err := manifest.Load(manifest.DefaultPath(stateDir))
	return err == nil && len(mf.Adapters) > 0
}

// claudeSettingsFiles lists the Claude settings files of this machine's
// sessions once each: the one the environment names (CLAUDE_CONFIG_DIR,
// else ~/.claude) when its directory exists, ~/.claude/settings.json when
// it exists, and the settings file of each config_dir a binding or a manifest
// entry records when its directory exists.
func claudeSettingsFiles(stateDir string, all []binding.Binding) []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	dirs = append(dirs, harness.ClaudeConfigDir(home))
	for _, b := range all {
		// A session that is not proven live may never read its file again.
		if state, _ := b.Liveness(); b.ConfigDir != "" && state == binding.Live {
			dirs = append(dirs, b.ConfigDir)
		}
	}
	for _, settings := range manifestClaudeSettings(stateDir) {
		dirs = append(dirs, filepath.Dir(settings))
	}
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if !filepath.IsAbs(dir) || seen[canonicalDir(dir)] {
			return
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return
		}
		seen[canonicalDir(dir)] = true
		out = append(out, filepath.Join(filepath.Clean(dir), "settings.json"))
	}
	for _, dir := range dirs {
		add(dir)
	}
	if home != "" {
		if dir := filepath.Join(home, ".claude"); fileExists(filepath.Join(dir, "settings.json")) {
			add(dir)
		}
	}
	return out
}

// manifestClaudeSettings lists the settings file of each Claude entry in
// the manifest under stateDir.
func manifestClaudeSettings(stateDir string) []string {
	settingsOf := claudeTargetSettings(stateDir)
	var out []string
	for _, target := range settingsOf.targets {
		if _, settings, err := settingsOf.lookup(target); err == nil {
			out = append(out, settings)
		}
	}
	return out
}

// canonicalDir is dir with its symlinks resolved, or clean when it does not
// resolve, so one directory reached two ways is checked once.
func canonicalDir(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hookFile is what doctor found in one Claude settings file.
type hookFile struct {
	Settings     string `json:"settings"`
	Approval     string `json:"approval"`
	ApprovalForm string `json:"approval_form,omitempty"`
	// Probe is the outcome of running the installed approval command:
	// ok, closed (it did not reach amq-remote), failed, or skipped.
	Probe           string `json:"probe,omitempty"`
	ProbeVersion    string `json:"probe_version,omitempty"`
	Stop            string `json:"stop"`
	StopForm        string `json:"stop_form,omitempty"`
	Guard           string `json:"guard"`
	DisableAllHooks bool   `json:"disable_all_hooks,omitempty"`
}

// Probe outcomes (hookFile.Probe).
const (
	probeOK      = "ok"
	probeClosed  = "closed"
	probeFailed  = "failed"
	probeSkipped = "skipped"
)

// doctorHooks checks the approval hook, the Stop hook and the wake dialog
// guard in each settings file, and that the installed approval command
// reaches amq-remote. A missing or stale entry fails, except a missing Stop
// hook while native is false, and a missing approval hook or guard while no
// binding and no machine agent exists (the hook's gate stays closed then, so
// nothing can reach Buzz yet), which are notes; disableAllHooks is a note,
// because a project or managed setting can turn hooks on again.
func doctorHooks(files []string, all []binding.Binding, native bool, machine agentInfo, fail, note reportFunc) []hookFile {
	home, _ := os.UserHomeDir()
	gateReport := note
	if len(all) > 0 || machine.State != agentMissing {
		gateReport = fail
	}
	envDir := canonicalDir(harness.ClaudeConfigDir(home))
	rows := make([]hookFile, 0, len(files))
	for _, path := range files {
		row := hookFile{Settings: path}
		// The installers write the file the environment names, so a remedy
		// for another file names its directory.
		fix := func(cmd string) string {
			dir := filepath.Dir(path)
			if canonicalDir(dir) == envDir {
				return "run `" + cmd + "`"
			}
			return "run `CLAUDE_CONFIG_DIR=" + shQuote(dir) + " " + cmd + "`"
		}
		approval, err := claude.PermissionHookState(path)
		if err != nil {
			fail("hooks", path, "cannot read it: "+err.Error(), "repair "+path)
			rows = append(rows, row)
			continue
		}
		row.Approval = approval
		optIn, err := claude.ReadHookOptIn(path)
		if err != nil {
			fail("hooks", path, "cannot read the owner's hook opt-in: "+err.Error(), "move that opt-in file away, then run the hook's install command")
		}
		switch {
		case approval == claude.HookMissing && optIn.Removed(claude.HookApproval):
			note("hooks", path, "the owner removed the AMQ approval hook with uninstall-approval-hook, so tool approvals of a Claude session that reads this file stay in the terminal", fix("amq-remote claude install-approval-hook"))
		case approval == claude.HookMissing:
			gateReport("hooks", path, "the AMQ approval hook (PermissionRequest) is not installed, so every tool approval of a Claude session that reads this file stays in the terminal", fix("amq-remote claude install-approval-hook"))
		default:
			row.ApprovalForm, _ = claude.PermissionHookForm(home, path)
			hookFormFinding(row.ApprovalForm, path, "approval (PermissionRequest)", fix("amq-remote claude install-approval-hook"), fail, note)
			if others, err := claude.OtherPermissionHooks(path); err == nil && len(others) > 0 {
				note("hooks", path, claude.OtherPermissionHookNote, "")
			}
			row.Probe, row.ProbeVersion = probeApprovalHook(path, probeGate(path, all, machine), fix("amq-remote claude install-approval-hook"), fail, note)
		}
		if row.Stop, err = claude.StopHookStateAt(path); err == nil && row.Stop == claude.HookMissing {
			// Only native mode needs it; on a mailbox-only machine it is advice.
			report := note
			if native && !optIn.Removed(claude.HookStop) {
				report = fail
			}
			report("hooks", path, "the AMQ Stop hook is not installed, so a native Claude session (/amq-remote --native) is admitted but never completes", fix("amq-remote claude install-stop-hook"))
		} else if err == nil {
			row.StopForm, _ = claude.StopHookForm(home, path)
			hookFormFinding(row.StopForm, path, "Stop", fix("amq-remote claude install-stop-hook"), fail, note)
		}
		var detail string
		row.Guard, detail = dialogguard.ClaudeAt(path)
		switch row.Guard {
		case dialogguard.Missing:
			gateReport("hooks", path, "the wake dialog guard is not installed, so a wake doorbell can answer an open approval dialog", fix("amq wake install-dialog-hook"))
		case dialogguard.Stale:
			gateReport("hooks", path, "the wake dialog guard has an older hook form, so the dialog hold does not close when Claude Code exits until the hook is reinstalled", fix("amq wake install-dialog-hook"))
		case dialogguard.Incomplete, dialogguard.Unreadable:
			detail, _, _ = strings.Cut(detail, "; remedy:")
			gateReport("hooks", path, "the wake dialog guard is "+row.Guard+" ("+detail+"), so a wake doorbell can answer an open approval dialog", fix("amq wake install-dialog-hook"))
		}
		if row.DisableAllHooks, _ = claude.HooksDisabledAt(path); row.DisableAllHooks {
			note("hooks", path, "it sets disableAllHooks, so Claude Code runs none of its hooks unless a project or managed setting turns them on", "remove disableAllHooks from "+path)
		}
		rows = append(rows, row)
	}
	return rows
}

// probeGate is an existing file that opens the approval command's gate for
// the probe: the file of a binding whose session reads path, else of any
// binding, else the machine agent's marker. "" when there is none; then no
// dialog can reach Buzz and the gate stays closed by design.
func probeGate(path string, all []binding.Binding, machine agentInfo) string {
	dir := canonicalDir(filepath.Dir(path))
	var gates []string
	for _, b := range all {
		if file, err := binding.FilePath(b.Name); err == nil {
			if b.ConfigDir != "" && canonicalDir(b.ConfigDir) == dir {
				gates = append([]string{file}, gates...)
			} else {
				gates = append(gates, file)
			}
		}
	}
	gates = append(gates, machine.Path)
	for _, gate := range gates {
		if gate != "" && fileExists(gate) {
			return gate
		}
	}
	return ""
}

// hookProbeTimeout bounds one run of the installed approval command.
const hookProbeTimeout = 5 * time.Second

// errNoShell is runHookProbe's refusal on a machine with no sh, where
// doctor cannot run the command as Claude Code does.
var errNoShell = errors.New("no sh to run the hook command with")

// runHookProbe runs the installed hook command through sh with the probe
// switch and the binding path gate set, and returns what it printed.
// Tests replace it.
var runHookProbe = func(command, gate string) (string, error) {
	if _, err := exec.LookPath("sh"); err != nil {
		return "", errNoShell
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = childWaitDelay
	cmd.Env = append(os.Environ(), hookProbeEnv+"=1", binding.EnvPath+"="+gate)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return string(out), err
}

// probeApprovalHook runs the one installed approval command in the settings
// file path silently: the receiver only prints its version. It returns the
// outcome and the version the command reached.
func probeApprovalHook(path, gate, remedy string, fail, note reportFunc) (string, string) {
	command, err := claude.PermissionHookCommand(path)
	if err != nil || command == "" || gate == "" {
		return probeSkipped, ""
	}
	out, err := runHookProbe(command, gate)
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	got, reached := strings.CutPrefix(strings.TrimSpace(line), "amq-remote ")
	switch {
	case errors.Is(err, errNoShell):
		return probeSkipped, ""
	case err != nil:
		fail("hooks", path, "the installed approval command failed when doctor ran it: "+err.Error(), remedy)
		return probeFailed, ""
	case reached:
		if got != version {
			note("hooks", path, fmt.Sprintf("the approval hook runs amq-remote %s; this doctor is %s", got, version), remedy)
		}
		return probeOK, got
	case line == "":
		fail("hooks", path, "the installed approval command did not reach amq-remote with a binding present: its gate stayed closed, or its amq-remote is older than this doctor, so no dialog reaches Buzz", remedy)
		return probeClosed, ""
	}
	fail("hooks", path, fmt.Sprintf("the installed approval command printed %q, not the amq-remote version", line), remedy)
	return probeFailed, ""
}

// shQuote single-quotes s for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// machineAgentRow is what doctor found about the machine agent.
type machineAgentRow struct {
	State    string `json:"state"`
	Name     string `json:"name,omitempty"`
	RelayKey bool   `json:"relay_key"`
	// A standalone agent's host, the first 8 characters of its key and of
	// the owner's key, its relay and relay state, its grant's end and its
	// refusal code.
	Host                string `json:"host,omitempty"`
	Agent               string `json:"agent,omitempty"`
	Owner               string `json:"owner,omitempty"`
	Relay               string `json:"relay,omitempty"`
	RelayState          string `json:"relay_state,omitempty"`
	GrantEnd            string `json:"grant_end,omitempty"`
	Refusal             string `json:"refusal,omitempty"`
	OutboxRecords       int    `json:"outbox_records,omitempty"`
	OutboxOldestUnknown string `json:"outbox_oldest_unknown,omitempty"`
}

// Doctor's bounds for a standalone machine agent: a relay not
// authenticated for longer fails, and a grant that ends sooner is noted.
const (
	standaloneRelayGrace    = time.Minute
	standaloneGrantNotice   = 14 * 24 * time.Hour
	standaloneUnknownNotice = 24 * time.Hour
)

// doctorMachineAgent checks that the machine agent runs (its marker is
// fresh), has a relay, and that Buzz Desktop starts it when it opens; a
// standalone agent has its own checks.
func doctorMachineAgent(machine agentInfo, fail, note reportFunc) machineAgentRow {
	row := machineAgentRow{State: machine.State}
	if machine.standalone() {
		return doctorStandalone(machine, row, time.Now(), fail, note)
	}
	if machine.State == agentMissing {
		note("machine_agent", "", "this machine has no AMQ Remote machine agent, so no session gets a channel of its own", machineSetupSteps)
		return row
	}
	row.Name = nonEmpty(machine.Display, "AMQ Remote @ "+nonEmpty(binding.Machine(), "this machine"))
	row.RelayKey = machine.RelayKey != ""
	if machine.State != agentRunning {
		why := "its marker is not fresh"
		if machine.State == agentIdle {
			why = "its worker is gone, and only a message to the agent starts a worker again"
		}
		fail("machine_agent", row.Name, "the machine agent is "+machine.State+": "+why+", so it makes no channel and posts no dialog",
			"set up the machine agent inside amq-remote serve: "+machineSetupSteps)
	} else if !row.RelayKey {
		fail("machine_agent", row.Name, "the machine agent records no relay key: it started without a usable BUZZ_RELAY_URL, so it makes no channel",
			"set up the machine agent inside amq-remote serve: "+machineSetupSteps)
	}
	return row
}

// doctorStandalone checks the machine agent that runs inside amq-remote
// serve, from its enrolled credential and its status file: it serves, its
// relay is authenticated, and its grant does not end soon. Buzz Desktop has
// no part in it.
func doctorStandalone(machine agentInfo, row machineAgentRow, now time.Time, fail, note reportFunc) machineAgentRow {
	row.Name = nonEmpty(machine.Display, "AMQ Remote @ "+nonEmpty(binding.Machine(), "this machine"))
	row.Host, row.Agent, row.RelayKey = hostStandalone, short(machine.PubKey), machine.RelayKey != ""
	st := machine.Status
	if st != nil {
		row.Relay, row.RelayState, row.GrantEnd, row.Refusal = st.RelayURL, st.RelayState, st.GrantNotAfter, machine.refusal()
	}
	var end, unknown time.Time
	if home, err := amqhome.Dir(); err == nil {
		if cred, err := machinecred.Load(home, machine.PubKey); err == nil {
			row.Owner, row.Relay, row.GrantEnd = short(cred.Tag.OwnerPubKey), buzzop.NormalizeRelay(cred.Relay), grantEnd(cred)
			if n, err := cred.Tag.NotAfter(); err == nil {
				end = time.Unix(n, 0)
			}
			if path, err := binding.Path(); err == nil {
				dir := machineOutboxDir(filepath.Join(filepath.Dir(path), "acp"), buzzop.RelayKey(cred.Relay), machine.PubKey)
				if stats, err := outbox.Stat(dir); err == nil {
					row.OutboxRecords = stats.Records
					if stats.OldestUnknown != 0 {
						unknown = time.Unix(stats.OldestUnknown, 0)
						row.OutboxOldestUnknown = unknown.UTC().Format(time.RFC3339)
					}
				}
			}
		}
	}
	switch {
	case !machine.serves():
		// The line ends with the user's next step.
		fail("machine_agent", row.Name, "the machine agent runs inside amq-remote serve and is not running: "+machine.notServingLine(), "")
	case st != nil && notAuthenticatedFor(st, now) > standaloneRelayGrace:
		fail("machine_agent", row.Name, fmt.Sprintf("the machine agent's relay %s has not been authenticated for more than a minute (%s%s), so no channel is made and no owner message arrives",
			st.RelayURL, nonEmpty(st.RelayState, "no state"), errSuffix(st.RelayError)), "check the network and the relay "+st.RelayURL+"; amq-remote serve reconnects by itself")
	}
	if !end.IsZero() && end.After(now) && end.Sub(now) < standaloneGrantNotice {
		note("machine_agent", row.Name, "the owner's grant for the machine agent ends "+row.GrantEnd,
			"run `amq-remote machine request`, have the owner sign it with `amq-owner-sign --machine FILE --out FILE --relay "+row.Relay+"`, then run `amq-remote machine enroll --grant FILE`")
	}
	if !unknown.IsZero() && now.Sub(unknown) > standaloneUnknownNotice {
		note("machine_agent", row.Name, "the machine agent's outbox holds a relay write with no known outcome since "+row.OutboxOldestUnknown+"; it stays until its outcome is known", "")
	}
	return row
}

// notAuthenticatedFor is how long the status says the relay connection has
// not been authenticated; zero while it is, or when the time does not read.
func notAuthenticatedFor(st *machineStatus, now time.Time) time.Duration {
	at, err := time.Parse(time.RFC3339, st.NotAuthenticatedSince)
	if err != nil {
		return 0
	}
	return now.Sub(at)
}

// doctorChannels checks the machine agent's channel record of each binding:
// it must be active. A channel being made, and a suspended one, is a note.
func doctorChannels(all []binding.Binding, fail, note reportFunc) map[string]string {
	out := map[string]string{}
	for _, b := range all {
		ch, ok := acp.MachineChannelOf(b)
		if !ok {
			continue
		}
		out[b.Name] = ch.State
		subject := nonEmpty(b.Label, b.Name)
		switch ch.State {
		case "active":
		case "creating":
			note("channel", subject, "the machine agent is making the channel of binding "+b.Name, "wait a minute, then run doctor again")
		case "suspended":
			note("channel", subject, "the channel of binding "+b.Name+" is suspended: "+ch.Error+"; the session's dialogs, turns and answers are not sent there",
				"undo it in Buzz, or run `amq-remote channel reset "+b.Name+"` for a new channel")
		default:
			remedy := "run `amq-remote channel reset " + b.Name + "`"
			if ch.Retry {
				remedy = "AMQ tries again in 10 minutes; to start over now, " + remedy
			}
			fail("channel", subject, "the channel of binding "+b.Name+" is "+ch.State+errSuffix(ch.Error), remedy)
		}
	}
	return out
}

// mailboxRow is what doctor found about one mailbox binding.
type mailboxRow struct {
	Name         string `json:"name"`
	Root         string `json:"root"`
	Handle       string `json:"handle"`
	Registration string `json:"registration"`
	LiveWake     *bool  `json:"live_wake,omitempty"`
	ImageStatus  string `json:"image_status,omitempty"`
}

// doctorMailboxes checks each mailbox binding of a session that may still
// run: its handle is registered, and `amq wake check` sees a live wake on
// the current image, so a Buzz message rings the session.
func doctorMailboxes(all []binding.Binding, fail, note reportFunc) []mailboxRow {
	var rows []mailboxRow
	for _, b := range all {
		if !b.Mailbox() {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Ended {
			continue
		}
		row := mailboxRow{Name: b.Name, Root: b.Root, Handle: b.Handle}
		subject := nonEmpty(b.Label, b.Name)
		row.Registration, _ = roster.AgentRegistration(route.Walker{}, b.Root, b.Handle)
		if row.Registration == "unregistered" {
			fail("mailbox", subject, fmt.Sprintf("handle %q is not registered in %s, so senders are warned and --strict sends refuse", b.Handle, b.Root),
				"start the session with `amq launch` or `amq coop exec`, which register its handle")
		}
		wake, err := doctorWakeCheck(b.Root, b.Handle)
		if err != nil {
			note("mailbox", subject, "cannot run `amq wake check`: "+err.Error(), "run `amq wake check --me "+b.Handle+" --root "+shQuote(b.Root)+"`")
			rows = append(rows, row)
			continue
		}
		row.LiveWake, row.ImageStatus = &wake.LiveWake, wake.ImageStatus
		remedy := func(fallback string) string {
			if wake.NextAction != "" {
				return wake.NextAction
			}
			return fallback
		}
		switch {
		case !wake.LiveWake:
			fail("mailbox", subject, fmt.Sprintf("handle %q has no live wake (%s), so a Buzz message waits in its inbox until the session drains", b.Handle, nonEmpty(wake.WakeStatus, "no wake")),
				remedy("start the session with `amq launch`, which starts its wake"))
		case wakeCannotReadDialogEntries(wake.RunningVersion):
			note("mailbox", subject, fmt.Sprintf("the wake of handle %q runs amq %s, which cannot read the dialog marker form of %s or later", b.Handle, wake.RunningVersion, dialogguard.EntriesSince),
				"restart that session (exit and resume it)")
		case wake.ImageStatus == "different" || wake.ImageStatus == "outdated":
			fail("mailbox", subject, fmt.Sprintf("the wake of handle %q runs another AMQ image than the installed one (image %s)", b.Handle, wake.ImageStatus),
				remedy("run `amq wake check --me "+b.Handle+" --root "+shQuote(b.Root)+"` and follow its action"))
		}
		rows = append(rows, row)
	}
	return rows
}

// wakeView is the part of `amq wake check --json` doctor reads.
type wakeView struct {
	LiveWake       bool   `json:"live_wake"`
	WakeStatus     string `json:"wake_status"`
	ImageStatus    string `json:"image_status"`
	NextAction     string `json:"next_action"`
	RunningVersion string `json:"running_version"`
}

// wakeCannotReadDialogEntries reports a wake of a known AMQ release that
// reads only the single-file dialog marker.
func wakeCannotReadDialogEntries(version string) bool {
	reads, known := dialogguard.WakeReadsEntries(version)
	return known && !reads
}

// wakeCheckTimeout bounds one `amq wake check`.
const wakeCheckTimeout = 15 * time.Second

// doctorWakeCheck runs the installed `amq wake check` for handle in root.
// It runs the amq binary, not this process: the check compares the wake's
// image with the running amq's own. Tests replace it.
var doctorWakeCheck = func(root, handle string) (wakeView, error) {
	amq, err := installedAMQ()
	if err != nil {
		return wakeView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wakeCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, amq, "wake", "check", "--me", handle, "--root", root, "--json")
	cmd.WaitDelay = childWaitDelay
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return wakeView{}, err
	}
	var v wakeView
	if err := json.Unmarshal(out, &v); err != nil {
		return wakeView{}, fmt.Errorf("read its output: %w", err)
	}
	return v, nil
}

// installedAMQ is the amq beside this amq-remote, symlinks resolved, else
// the amq on PATH.
func installedAMQ() (string, error) { return claude.SiblingBinary("amq") }
