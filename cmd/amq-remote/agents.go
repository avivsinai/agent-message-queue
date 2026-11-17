package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

// The machine agent runs inside amq-remote serve. It is known from its
// enrolled credential and the marker it rewrites while it runs,
// <binding dir>/acp/agents/_machine/<pubkey>.json with host "standalone",
// which records its process identity (acp.AgentState). A marker with
// another host is a file no agent writes, and is ignored.
const (
	agentRunning = acp.AgentRunning // the marker's worker runs
	agentOffline = acp.AgentOffline // a marker or credential exists, no process of its runs
	agentMissing = "missing"        // no marker and no enrolled credential

	machineMarkerDir = "_machine"
	maxMarkerBytes   = 64 * 1024
)

// agentInfo is what the markers and the enrolled credential say about the
// machine agent.
type agentInfo struct {
	State   string
	Display string // the display name of the newest marker
	// PubKey and Path are those of the marker in the best state, the newest
	// among equals: its file name (the agent's public key) and its file.
	// With no marker, PubKey is the enrolled credential's key.
	PubKey, Path string
	// Status is the agent's status file; nil when it has none.
	Status *machineStatus
}

// hostStandalone is the marker host of a machine agent that runs inside
// amq-remote serve.
const hostStandalone = "standalone"

// machineSetupSteps is the one-time setup of the machine agent, for a
// machine that has none inside amq-remote serve.
const machineSetupSteps = "run `amq-remote machine request > request.txt`; the owner signs it in their own terminal with `amq-owner-sign --machine request.txt --out grant.json --relay <wss-url>`; then run `amq-remote machine enroll --grant grant.json` and `amq-remote service install`"

// machineOf reports this machine's machine agent. With no marker, an
// enrolled machine credential is an agent that has not run: it reads
// offline. The agent carries its status file.
func machineOf(now time.Time) agentInfo {
	info := machineMarkerOf(now)
	home, err := amqhome.Dir()
	if err != nil {
		return info
	}
	if info.State == agentMissing {
		agents, err := machinecred.Agents(home)
		if err != nil || len(agents) == 0 {
			return info
		}
		info.State, info.PubKey = agentOffline, agents[0]
	}
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(home, "remote", "machine", info.PubKey, machineStatusFile), maxMarkerBytes)
	var st machineStatus
	if err == nil && json.Unmarshal(raw, &st) == nil && !statusOfDeadServe(st) {
		info.Status = &st
	}
	return info
}

// statusOfDeadServe reports a status file whose writer is proven gone: its
// pid does not run. Such a file says nothing about the agent now. A pid
// whose process runs, or that cannot be checked, keeps the file.
func statusOfDeadServe(st machineStatus) bool {
	state, _ := launch.InspectHolder(launch.LocalHolder(st.PID, "", ""))
	return state == launch.LeaseStale
}

// serves reports whether the machine agent serves sessions now: its marker
// reads running and its status does not say it waits or refused. The
// marker reads running while its serve process lives, even after the serve
// stopped the agent.
func (a agentInfo) serves() bool {
	return a.State == agentRunning && (a.Status == nil || a.Status.State == "serving")
}

// refusal is the agent's refusal code, or "".
func (a agentInfo) refusal() string {
	if a.Status != nil && a.Status.State == "refused" {
		return a.Status.Code
	}
	return ""
}

// notServingLine says why the agent does not serve and the user's next
// step: its status line, else the grant's refusal when it does not load,
// else to start the login service.
func (a agentInfo) notServingLine() string {
	if a.Status != nil && a.Status.State != "serving" && a.Status.Line != "" {
		return a.Status.Line
	}
	if home, err := amqhome.Dir(); err == nil && a.PubKey != "" {
		if _, err := machinecred.Load(home, a.PubKey); err != nil {
			return noAuthorityLine(a.PubKey, err)
		}
	}
	return "run amq-remote up."
}

// notServing is the line attach prints for an agent that does not serve.
func (a agentInfo) notServing() string {
	return "The machine agent runs inside amq-remote serve and is not running: " + a.notServingLine()
}

// stateRank orders agent states: the best state of any marker wins.
var stateRank = map[string]int{agentMissing: 0, agentOffline: 1, agentRunning: 2}

// machineMarkerOf reports the machine agent as its markers read at now.
func machineMarkerOf(now time.Time) agentInfo {
	info := agentInfo{State: agentMissing}
	path, err := binding.Path()
	if err != nil {
		return info
	}
	dir := filepath.Join(filepath.Dir(path), "acp", "agents", machineMarkerDir)
	entries, _ := os.ReadDir(dir)
	var newest, bestAt time.Time
	bestRank := -1
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, e.Name()), maxMarkerBytes)
		var m struct {
			DisplayName string                `json:"display_name"`
			Updated     string                `json:"updated"`
			Worker      launch.HolderIdentity `json:"worker"`
			Host        string                `json:"host"`
		}
		if err != nil || json.Unmarshal(raw, &m) != nil || m.Host != hostStandalone {
			continue
		}
		updated, err := time.Parse(time.RFC3339, m.Updated)
		if err != nil {
			continue
		}
		state := acp.AgentState(m.Updated, m.Worker, now)
		if stateRank[state] > stateRank[info.State] {
			info.State = state
		}
		if updated.After(newest) {
			newest, info.Display = updated, m.DisplayName
		}
		if rank := stateRank[state]; rank > bestRank || rank == bestRank && updated.After(bestAt) {
			bestRank, bestAt = rank, updated
			info.PubKey, info.Path = strings.TrimSuffix(e.Name(), ".json"), filepath.Join(dir, e.Name())
		}
	}
	return info
}

// channelWait bounds how long attach waits for the machine agent to make
// the session's channel.
const channelWait = 15 * time.Second

// attachView is what attach reports for one session: the machine agent
// and the session's channel.
type attachView struct {
	name, label string
	reused      bool
	machine     agentInfo
	// unproven is the liveness evidence of a session that is not proven
	// live, which the machine agent makes no channel for; "" when it is.
	unproven string
	// channel is the machine agent's record of the session's channel; nil
	// when it has none.
	channel *acp.MachineChannel
	// reviewer is the automatic approvals reviewer of a Codex thread, which
	// answers its approvals so none reach Buzz; "" for the user.
	reviewer string
}

// viewOf reads what attach reports for the binding called name. While the
// machine agent runs and serves the session, and the session is proven live
// (the machine agent's own rule), it waits up to channelWait for the
// channel to leave creating.
func viewOf(b binding.Binding, name string, reused bool) attachView {
	now := time.Now()
	v := attachView{name: name, label: b.Label, reused: reused, machine: machineOf(now)}
	if bound, err := binding.ReadNamed(name); err == nil {
		b = bound
	}
	// The machine agent's own judgement: a Codex thread is live while its
	// app-server daemon has it loaded, with no session process to hold.
	if state, evidence := newCodexView().Liveness(b); state != binding.Live {
		v.unproven = string(state) + ": " + evidence
	}
	deadline := now.Add(channelWait)
	for {
		if c, ok := acp.MachineChannelOf(b); ok {
			v.channel = &c
		}
		if !v.machine.serves() || v.unproven != "" || (v.channel != nil && v.channel.State != "creating") || !time.Now().Before(deadline) {
			return v
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// channelState is the channel state attach reports: the record's, or
// creating while the machine agent runs, the session is proven live, and no
// record is written yet.
func (v attachView) channelState() string {
	switch {
	case v.channel != nil:
		return v.channel.State
	case v.machine.serves() && v.unproven == "":
		return "creating"
	}
	return ""
}

// connected prints what attach did and the one thing the owner still has to
// do: open the session's channel, start the machine agent, or set it up
// inside amq-remote serve.
func connected(w io.Writer, v attachView) {
	say(w, "Connected: %s.", nonEmpty(v.label, v.name))
	machine := nonEmpty(v.machine.Display, "AMQ Remote @ "+nonEmpty(binding.Machine(), "this machine"))
	serves := v.machine.serves()
	switch state := v.channelState(); {
	case state == "suspended":
		say(w, "This session's channel %q is suspended: %s. Nothing is sent there until that changes in Buzz; for a new channel run amq-remote channel reset %s.", v.channel.Name, v.channel.Error, v.name)
	case serves && state == "active":
		say(w, "Open the %q channel in Buzz.", v.channel.Name)
	case serves && state == "creating":
		say(w, "AMQ Remote is making the channel; it appears in Buzz within a minute.")
	case v.machine.State != agentMissing && !serves:
		// Nothing makes, looks for or retries a channel until serve runs it.
		say(w, "%s", v.machine.notServing())
	case v.machine.State != agentMissing && state == "" && v.unproven != "":
		say(w, "%s makes a channel only for a session proven live, and this session is not (%s).", machine, v.unproven)
	case state == "uncertain":
		say(w, "Buzz did not confirm the channel (%s). AMQ keeps looking for it and will not make a second one. If it never appears, run amq-remote channel reset %s.", v.channel.Error, v.name)
	case state == "failed" && v.channel.Retry:
		say(w, "Buzz refused the channel (%s). AMQ tries again in 10 minutes.", v.channel.Error)
	case state == "failed":
		say(w, "Buzz refused the channel (%s). Run amq-remote channel reset %s.", v.channel.Error, v.name)
	default:
		say(w, "This machine has no AMQ Remote agent yet. One-time setup: %s. Each session then gets its own private channel.", machineSetupSteps)
	}
	if v.reviewer != "" {
		say(w, "%s.", launch.CodexReviewerLine(v.reviewer))
	}
}

// connectedJSON adds the machine agent and channel fields to an attach
// result.
func connectedJSON(out map[string]any, v attachView) map[string]any {
	out["label"], out["reused"], out["machine"] = v.label, v.reused, v.machine.State
	if v.machine.State != agentMissing {
		out["machine_name"] = nonEmpty(v.machine.Display, "AMQ Remote @ "+binding.Machine())
		if code := v.machine.refusal(); code != "" {
			out["machine_refusal"] = code
		}
		if !v.machine.serves() {
			out["machine_line"] = v.machine.notServing()
		}
	}
	if v.channel == nil && v.unproven != "" {
		out["unproven"] = v.unproven
	}
	if v.reviewer != "" {
		out["approval_reviewer"] = v.reviewer
	}
	if state := v.channelState(); state != "" {
		out["channel"] = state
		if v.channel != nil {
			out["channel_name"] = v.channel.Name
			if state == "uncertain" || state == "failed" || state == "suspended" {
				out["channel_error"], out["channel_retry"] = v.channel.Error, v.channel.Retry
			}
		}
	}
	return out
}

// unknownHint names the command for sessions whose liveness is unknown.
func unknownHint(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Not proven live or ended: %s. When such a session is gone, run amq-remote detach --name <name>.", strings.Join(names, ", "))
}
