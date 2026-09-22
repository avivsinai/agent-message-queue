package claude

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// maxDiscoverEntries bounds the registry scan: ~/.claude/sessions holds one
// entry per live or recently live session.
const maxDiscoverEntries = 512

// discoverer lists attachable Claude Code sessions (bead 611.13): every
// <pid>.json registry entry under ~/.claude/sessions that is interactive,
// has a live pid, and exposes a messaging socket. It reads through the
// same bounded, lstat-gated registry reader as Attach and never attaches.
type discoverer struct {
	// home overrides the Claude home for tests; empty = os.UserHomeDir().
	home string
}

func (d discoverer) Discover(_ context.Context, _ registry.DiscoverRequest) ([]registry.Candidate, error) {
	home := d.home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, nil // no home: nothing to discover
		}
		home = h
	}
	entries, err := os.ReadDir(claudeSessionsDir(home))
	if err != nil {
		return nil, nil // no Claude sessions directory: nothing to discover
	}
	var pids []int
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 || strconv.Itoa(pid) != name {
			continue
		}
		pids = append(pids, pid)
		if len(pids) == maxDiscoverEntries {
			break
		}
	}
	sort.Ints(pids)
	var out []registry.Candidate
	for _, pid := range pids {
		reg, err := readSessionRegistry(home, pid)
		if err != nil || reg == nil || reg.MessagingSocketPath == "" {
			continue
		}
		if reg.Kind != "" && reg.Kind != "interactive" {
			continue
		}
		if alive, err := pidAlive(pid); err != nil || !alive {
			continue
		}
		// Target is always claude:<pid>: unique per process and protocol
		// valid. Session names can repeat across sessions, so the name is
		// display text only (codex 611.13 consult, requirement 3).
		cfg, _ := json.Marshal(map[string]int{"pid": pid})
		out = append(out, registry.Candidate{Kind: "claude", Target: "claude:" + strconv.Itoa(pid), Display: reg.Name, Config: cfg})
	}
	return out, nil
}

func init() {
	registry.RegisterDiscoverer("claude", discoverer{})
}
