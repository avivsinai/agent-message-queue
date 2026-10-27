package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A session's Buzz agent is known from the markers amq-acp writes for it: a
// pinned agent rewrites <binding dir>/acp/agents/<name>/<pubkey>.json on
// every tick, the helper writes agents/_helper/<pubkey>.json, and the
// machine agent agents/_machine/<pubkey>.json. Each marker records its
// worker's and parent buzz-acp's process identity (acp.AgentState). AMQ
// never reads Buzz Desktop's own state.
const (
	agentRunning = acp.AgentRunning // the marker's worker runs
	agentIdle    = acp.AgentIdle    // the worker is gone, its parent buzz-acp runs
	agentOffline = acp.AgentOffline // markers exist, no process of theirs runs
	agentMissing = "missing"        // no marker: the agent was never started

	helperMarkerDir  = "_helper"
	machineMarkerDir = "_machine"
	maxMarkerBytes   = 64 * 1024
)

// agentInfo is what the markers say about one agent directory.
type agentInfo struct {
	State   string
	Display string // the display name of the newest marker
	// Ready is true for a running helper whose control channel is set up
	// with the owner in it, so it can send Desktop a create draft.
	Ready bool
}

// helperOf reports this machine's helper agent.
func helperOf(now time.Time) agentInfo { return agentOf(helperMarkerDir, now) }

// machineOf reports this machine's machine agent.
func machineOf(now time.Time) agentInfo { return agentOf(machineMarkerDir, now) }

// stateRank orders agent states: the best state of any marker wins.
var stateRank = map[string]int{agentMissing: 0, agentOffline: 1, agentIdle: 2, agentRunning: 3}

// agentOf reports the Buzz agent pinned to the binding called name (or the
// helper or machine agent, under its marker directory), as its markers read
// at now.
func agentOf(dirName string, now time.Time) agentInfo {
	info := agentInfo{State: agentMissing}
	path, err := binding.Path()
	if err != nil {
		return info
	}
	dir := filepath.Join(filepath.Dir(path), "acp", "agents", dirName)
	entries, _ := os.ReadDir(dir)
	var newest time.Time
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, e.Name()), maxMarkerBytes)
		var m struct {
			DisplayName string `json:"display_name"`
			Updated     string `json:"updated"`
			Channel     string `json:"channel"`
			// OwnerAdmin is the helper's control channel setup; an older
			// helper wrote only OwnerAdded, and it drafts too.
			OwnerAdmin bool                  `json:"owner_admin"`
			OwnerAdded bool                  `json:"owner_added"`
			Worker     launch.HolderIdentity `json:"worker"`
			Parent     launch.HolderIdentity `json:"parent"`
		}
		if err != nil || json.Unmarshal(raw, &m) != nil {
			continue
		}
		updated, err := time.Parse(time.RFC3339, m.Updated)
		if err != nil {
			continue
		}
		state := acp.AgentState(m.Updated, m.Worker, m.Parent, now)
		if stateRank[state] > stateRank[info.State] {
			info.State = state
		}
		if state == agentRunning {
			info.Ready = info.Ready || m.Channel != "" && (m.OwnerAdmin || m.OwnerAdded)
		}
		if updated.After(newest) {
			newest, info.Display = updated, m.DisplayName
		}
	}
	return info
}

// agentDisplay is the name the session's agent shows in Buzz: its marker's,
// else the name the helper drafts with (the label), else the name setup
// gives it.
func agentDisplay(agent agentInfo, label, name string) string {
	return nonEmpty(agent.Display, nonEmpty(label, "AMQ: "+name))
}

// helperDrafts reports whether the helper will send Desktop a create draft
// for b, by the helper's own rule: it is ready, and b's session is proven
// live. Only a session with no agent is drafted.
func helperDrafts(b binding.Binding, helper agentInfo) bool {
	state, _ := b.Liveness()
	return helper.Ready && state == binding.Live
}

// channelWait bounds how long attach waits for the machine agent to make
// the session's channel.
const channelWait = 15 * time.Second

// attachView is what attach reports for one session: its own Buzz agent,
// the helper, and the machine agent with the session's channel.
type attachView struct {
	name, label            string
	reused                 bool
	agent, helper, machine agentInfo
	// draft is whether the helper opens Desktop's create dialog for it.
	draft bool
	// served is whether the machine agent serves the session: no agent of
	// its own runs or is idle.
	served bool
	// unproven is the liveness evidence of a session that is not proven
	// live, which the machine agent makes no channel for; "" when it is.
	unproven string
	// channel is the machine agent's record of the session's channel; nil
	// when it has none.
	channel *acp.MachineChannel
}

// viewOf reads what attach reports for the binding called name. While the
// machine agent runs and serves the session, and the session is proven live
// (the machine agent's own rule), it waits up to channelWait for the
// channel to leave creating.
func viewOf(b binding.Binding, name string, reused bool) attachView {
	now := time.Now()
	v := attachView{name: name, label: b.Label, reused: reused, agent: agentOf(name, now), helper: helperOf(now), machine: machineOf(now)}
	v.draft = helperDrafts(b, v.helper)
	v.served = v.agent.State != agentRunning && v.agent.State != agentIdle
	if !v.served {
		return v
	}
	if bound, err := binding.ReadNamed(name); err == nil {
		b = bound
	}
	if state, evidence := b.Liveness(); state != binding.Live {
		v.unproven = string(state) + ": " + evidence
	}
	deadline := now.Add(channelWait)
	for {
		if c, ok := acp.MachineChannelOf(b); ok {
			v.channel = &c
		}
		if v.machine.State != agentRunning || v.unproven != "" || (v.channel != nil && v.channel.State != "creating") || !time.Now().Before(deadline) {
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
	case !v.served:
		return ""
	case v.channel != nil:
		return v.channel.State
	case v.machine.State == agentRunning && v.unproven == "":
		return "creating"
	}
	return ""
}

// connected prints what attach did and the one thing the owner still has to
// do: DM the session's own agent, open its channel, wait for the channel,
// reset it, set up or start the machine agent, or start the session's agent.
func connected(w io.Writer, v attachView) {
	say(w, "Connected: %s.", nonEmpty(v.label, v.name))
	display := agentDisplay(v.agent, v.label, v.name)
	machine := nonEmpty(v.machine.Display, "AMQ Remote @ "+nonEmpty(binding.Machine(), "this machine"))
	switch state := v.channelState(); {
	case v.agent.State == agentRunning:
		say(w, "DM %q in Buzz.", display)
	case v.agent.State == agentIdle:
		say(w, "DM %q in Buzz; Buzz starts it on the next message.", display)
	case v.machine.State == agentRunning && state == "active":
		say(w, "Open the %q channel in Buzz.", v.channel.Name)
	case v.machine.State == agentRunning && state == "creating":
		say(w, "Buzz is making the channel; it appears within a minute.")
	case v.machine.State != agentMissing && state == "" && v.unproven != "":
		say(w, "%s makes a channel only for a session proven live, and this session is not (%s).", machine, v.unproven)
	case state == "uncertain":
		say(w, "Buzz did not confirm the channel (%s). AMQ keeps looking for it and will not make a second one. If it never appears, run amq-remote channel reset %s.", v.channel.Error, v.name)
	case state == "failed" && v.channel.Retry:
		say(w, "Buzz refused the channel (%s). AMQ tries again in 10 minutes.", v.channel.Error)
	case state == "failed":
		say(w, "Buzz refused the channel (%s). Run amq-remote channel reset %s.", v.channel.Error, v.name)
	case v.agent.State == agentOffline && v.machine.State == agentMissing:
		say(w, "Its Buzz agent %q is offline: DM it first; if it does not answer within a minute, start it in Buzz Desktop (Agents, the agent, Start).", display)
	case v.machine.State == agentMissing && v.draft:
		say(w, "Buzz Desktop opens \"Create agent\" for it: pick AMQ Remote, then Create and start.")
	case v.machine.State == agentMissing && v.helper.State != agentMissing:
		say(w, "This session has no Buzz agent yet: run amq-acp setup --session %s and import the file in Buzz Desktop.", v.name)
	case v.machine.State == agentMissing:
		say(w, "This machine has no AMQ Remote agent yet: run amq-acp setup --machine and import the file in Buzz Desktop, once per machine. Each session then gets its own private channel.")
	default:
		say(w, "Start %q in Buzz Desktop (Agents, the agent, Start); it then makes this session's channel.", machine)
	}
}

// connectedJSON adds the agent and channel fields to an attach result.
func connectedJSON(out map[string]any, v attachView) map[string]any {
	out["label"], out["reused"], out["agent"], out["helper"], out["draft"] = v.label, v.reused, v.agent.State, v.helper.State, v.draft
	out["machine"] = v.machine.State
	if v.agent.State != agentMissing {
		out["agent_name"] = agentDisplay(v.agent, v.label, v.name)
	}
	if v.machine.State != agentMissing {
		out["machine_name"] = nonEmpty(v.machine.Display, "AMQ Remote @ "+binding.Machine())
	}
	if v.served && v.channel == nil && v.unproven != "" {
		out["unproven"] = v.unproven
	}
	if state := v.channelState(); state != "" {
		out["channel"] = state
		if v.channel != nil {
			out["channel_name"] = v.channel.Name
			if state == "uncertain" || state == "failed" {
				out["channel_error"], out["channel_retry"] = v.channel.Error, v.channel.Retry
			}
		}
	}
	return out
}

// legacyNoticeFile records the old agents bindings already named, so the
// notice names each one once.
const legacyNoticeFile = "legacy-agents.json"

// legacyAgents returns the per-session Buzz agents whose bindings are now
// removed and that no notice has named yet: each agent that left a marker
// for the session, by its display name, and the agent of a binding made
// before sessions had labels, which was made by hand and left none. With
// record, it records them as named, so the printed notice names each once;
// a JSON listing reports them without recording. Archiving does not remove
// an agent from Buzz Desktop, so the owner deletes it there.
func legacyAgents(record bool) []string {
	tombs, err := binding.Tombstones()
	if err != nil {
		return nil
	}
	path, err := binding.Path()
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	var named []string
	if raw, err := os.ReadFile(filepath.Join(dir, legacyNoticeFile)); err == nil {
		_ = json.Unmarshal(raw, &named)
	}
	var fresh, agents []string
	now := time.Now()
	for _, t := range tombs {
		if slices.Contains(named, t.Name) {
			continue
		}
		if _, err := binding.ReadNamed(t.Name); err == nil {
			continue // bound again: its agent serves the session
		}
		switch agent := agentOf(t.Name, now); {
		case agent.State != agentMissing:
			fresh, agents = append(fresh, t.Name), append(agents, agentDisplay(agent, t.Label, t.Name))
		case t.Label == "":
			fresh, agents = append(fresh, t.Name), append(agents, "AMQ: "+t.Name)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	if !record {
		return agents
	}
	raw, err := json.Marshal(append(named, fresh...))
	if err != nil {
		return nil
	}
	if _, err := fsq.WriteFileAtomic(dir, legacyNoticeFile, append(raw, '\n'), 0o600); err != nil {
		return nil
	}
	return agents
}

// sayLegacyAgents prints the one-time notice for old agents.
func sayLegacyAgents(w io.Writer, agents []string) {
	if len(agents) == 0 {
		return
	}
	say(w, "Delete these in Buzz Desktop (Agents, the agent, Delete): %s.", strings.Join(agents, ", "))
}

// unknownHint names the command for sessions whose liveness is unknown.
func unknownHint(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Not proven live or ended: %s. When such a session is gone, run amq-remote detach --name <name>.", strings.Join(names, ", "))
}
