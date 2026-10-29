package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// maxDiscoverEntries bounds the registry scan: the sessions registry holds
// one entry per live or recently live session.
const maxDiscoverEntries = 512

// discoverer lists attachable Claude Code sessions: every
// <pid>.json registry entry under the sessions registry of this process's
// config dir that is interactive, has a live pid, and exposes a messaging
// socket. It runs inside the session, so it takes the config dir from
// CLAUDE_CONFIG_DIR and records it in each candidate's config. It reads
// through the same bounded, lstat-gated registry reader as Attach and never
// attaches.
type discoverer struct {
	// home overrides the Claude home for tests; empty = os.UserHomeDir().
	home string
}

func (d discoverer) Discover(_ context.Context, _ registry.DiscoverRequest) ([]registry.Candidate, error) {
	home := d.home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		home = h
	}
	configDir, err := sessionConfigDir(home)
	if err != nil {
		return nil, err
	}
	sessions := registryDir(configDir)
	// The default dir is not recorded, so its config keeps the {"pid":N}
	// bytes existing manifest entries hold.
	recorded := ""
	if configDir != defaultConfigDir(home) {
		recorded = configDir
	}
	// Bounded enumeration: read the directory in batches and
	// stop after maxDiscoverEntries names, reporting the scan incomplete
	// instead of silently hiding live sessions behind stale rows.
	dir, err := openSessionsDir(sessions)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no Claude sessions directory: nothing to discover
		}
		return nil, fmt.Errorf("read %s: %w", sessions, err)
	}
	defer func() { _ = dir.Close() }()
	var pids []int
	scanned, incomplete := 0, false
	for !incomplete {
		entries, rerr := dir.ReadDir(128)
		for _, e := range entries {
			if scanned == maxDiscoverEntries {
				incomplete = true
				break
			}
			scanned++
			name, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok {
				continue
			}
			pid, err := strconv.Atoi(name)
			if err != nil || pid <= 0 || strconv.Itoa(pid) != name {
				continue
			}
			pids = append(pids, pid)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return nil, fmt.Errorf("read %s: %w", sessions, rerr)
			}
			break
		}
	}
	sort.Ints(pids)
	var out []registry.Candidate
	for _, pid := range pids {
		reg, err := readSessionRegistry(configDir, pid)
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
		// display text only.
		cfg, _ := json.Marshal(config{Pid: pid, ConfigDir: recorded})
		out = append(out, registry.Candidate{Kind: "claude", Target: "claude:" + strconv.Itoa(pid), Display: reg.Name, Config: cfg})
	}
	if incomplete {
		return out, fmt.Errorf("%w: examined the first %d entries of %s", registry.ErrIncomplete, maxDiscoverEntries, sessions)
	}
	return out, nil
}

func init() {
	registry.RegisterDiscoverer("claude", discoverer{})
}
