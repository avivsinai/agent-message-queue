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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A session's Buzz agent is known from the markers amq-acp writes for it: a
// pinned agent rewrites <binding dir>/acp/agents/<name>/<pubkey>.json on
// every tick, and the helper writes agents/_helper/<pubkey>.json. AMQ never
// reads Buzz Desktop's own state.
const (
	agentRunning = "running" // a marker was rewritten within agentMarkerFresh
	agentOffline = "offline" // markers exist, none rewritten recently
	agentMissing = "missing" // no marker: the agent was never started

	helperMarkerDir = "_helper"
	// agentMarkerFresh matches amq-acp: a tick can wait behind a slow sweep.
	agentMarkerFresh = 10 * time.Minute
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

// agentOf reports the Buzz agent pinned to the binding called name (or the
// helper, under its marker directory), as its markers read at now.
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
			OwnerAdmin bool `json:"owner_admin"`
			OwnerAdded bool `json:"owner_added"`
		}
		if err != nil || json.Unmarshal(raw, &m) != nil {
			continue
		}
		updated, err := time.Parse(time.RFC3339, m.Updated)
		if err != nil {
			continue
		}
		if info.State == agentMissing {
			info.State = agentOffline
		}
		if now.Sub(updated) <= agentMarkerFresh {
			info.State = agentRunning
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
// live (review-s5 P1-1). Only a session with no agent is drafted.
func helperDrafts(b binding.Binding, helper agentInfo) bool {
	state, _ := b.Liveness()
	return helper.Ready && state == binding.Live
}

// connected prints what attach did and the one thing the owner still has to
// do for this session's Buzz agent: nothing, start it, accept the helper's
// create dialog, or create it once.
func connected(w io.Writer, name, label string, agent, helper agentInfo, draft bool) {
	say(w, "Connected: %s.", nonEmpty(label, name))
	display := agentDisplay(agent, label, name)
	switch {
	case agent.State == agentRunning:
		say(w, "DM %q in Buzz.", display)
	case agent.State == agentOffline:
		say(w, "Its Buzz agent %q is not running: start it in Buzz Desktop (Agents, the agent, Start), then DM it. If you deleted it, run amq-acp setup --session %s and import the file.", display, name)
	case draft:
		say(w, "Buzz Desktop opens \"Create agent\" for it: pick AMQ Remote, then Create and start.")
	case helper.State == agentMissing:
		say(w, "This session has no Buzz agent yet: run amq-acp setup --session %s and import the file in Buzz Desktop. Tip: import the helper once (amq-acp setup --helper), and later sessions need only 3 clicks.", name)
	default:
		say(w, "This session has no Buzz agent yet: run amq-acp setup --session %s and import the file in Buzz Desktop.", name)
	}
}

// connectedJSON adds the agent fields to an attach result. draft is whether
// the helper will open Desktop's create dialog for the session.
func connectedJSON(out map[string]any, name, label string, reused bool, agent, helper agentInfo, draft bool) map[string]any {
	out["label"], out["reused"], out["agent"], out["helper"], out["draft"] = label, reused, agent.State, helper.State, draft
	if agent.State != agentMissing {
		out["agent_name"] = agentDisplay(agent, label, name)
	}
	return out
}

// legacyNoticeFile records the old agents bindings already named, so the
// notice names each one once.
const legacyNoticeFile = "legacy-agents.json"

// legacyAgents returns the agents of bindings made before sessions had labels
// whose bindings are now removed and that no notice has named yet. With
// record, it records them as named, so the printed notice names each once;
// a JSON listing reports them without recording. Such an agent was made by
// hand and never archives itself, so the owner deletes it.
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
	var fresh []string
	for _, t := range tombs {
		if t.Label == "" && !slices.Contains(named, t.Name) {
			fresh = append(fresh, t.Name)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	agents := make([]string, len(fresh))
	for i, n := range fresh {
		agents[i] = "AMQ: " + n
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
	say(w, "Old Buzz agents no longer bound to a session: %s.", strings.Join(agents, ", "))
	say(w, "Delete them in Buzz Desktop (Agents, the agent, Delete) and hide their DMs, as well as any agent you made by hand for AMQ Remote.")
}

// unknownHint names the command for sessions whose liveness is unknown.
func unknownHint(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Not proven live or ended: %s. When such a session is gone, run amq-remote detach --name <name>.", strings.Join(names, ", "))
}
