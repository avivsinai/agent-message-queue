package source

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type claudePermissionOwners struct {
	owners    map[string]board.Owner
	notReady  map[string]bool
	worktrees map[string]bool
	hint      board.Owner
}

func claudePermissionMapping(ctx context.Context, env board.Env) (claudePermissionOwners, error) {
	m := claudePermissionOwners{owners: map[string]board.Owner{}, notReady: map[string]bool{}, worktrees: map[string]bool{}}
	out, err := env.Run.Run(ctx, env.Dir, []string{"amq", "session", "list", "--json"})
	if err != nil {
		return m, fmt.Errorf("claude permission mapping: %w", err)
	}
	var listing struct {
		Sessions []struct {
			Name          string                                         `json:"name"`
			Conversations []struct{ Handle, Provider, ID, State string } `json:"conversations"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return m, fmt.Errorf("claude permission mapping: %w", err)
	}
	kinds := map[board.Owner]string{}
	for _, s := range listing.Sessions {
		if s.Name != "" && fsq.ValidateHandle(s.Name) != nil {
			continue
		}
		for _, c := range s.Conversations {
			if fsq.ValidateHandle(c.Handle) != nil {
				continue
			}
			owner := board.Owner{Kind: board.OwnerAgent, Session: s.Name, Handle: c.Handle, Display: c.Handle}
			if c.State == "ready" {
				if prior, ok := kinds[owner]; ok && prior != c.Provider {
					kinds[owner] = "ambiguous"
				} else {
					kinds[owner] = c.Provider
				}
			}
			if c.Provider != "claude" {
				continue
			}
			if _, ok := board.ClaudeSession(c.ID); !ok {
				continue
			}
			if c.State != "ready" {
				m.notReady[c.ID] = true
				continue
			}
			if old, exists := m.owners[c.ID]; exists && old != owner {
				owner = board.Owner{Kind: board.OwnerUnmapped, Display: "ambiguous Claude session " + c.ID}
			}
			m.owners[c.ID] = owner
		}
	}
	out, err = env.Run.Run(ctx, env.Dir, []string{"git", "worktree", "list", "--porcelain", "-z"})
	if err != nil {
		return m, fmt.Errorf("claude permission project worktrees: %w", err)
	}
	for _, field := range strings.Split(string(out), "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			real, err := filepath.EvalSymlinks(path)
			if err != nil {
				return m, fmt.Errorf("claude permission project worktree: %w", err)
			}
			m.worktrees[real] = true
		}
	}
	out, err = env.Run.Run(ctx, env.Dir, []string{"amq", "who", "--json"})
	if err != nil {
		return m, fmt.Errorf("claude permission owner hints: %w", err)
	}
	live, err := parseWho(out)
	if err != nil {
		return m, fmt.Errorf("claude permission owner hints: %w", err)
	}
	var agents, claude []board.Owner
	for key, row := range live {
		if !row.Active || key[1] == reservedHuman || fsq.ValidateHandle(key[1]) != nil || (key[0] != "" && fsq.ValidateHandle(key[0]) != nil) {
			continue
		}
		owner := board.Owner{Kind: board.OwnerAgent, Session: key[0], Handle: key[1], Display: key[1]}
		agents = append(agents, owner)
		if kinds[owner] == "claude" {
			claude = append(claude, owner)
		}
	}
	if len(claude) == 1 {
		m.hint = claude[0]
	} else if len(claude) == 0 && len(agents) == 1 && kinds[agents[0]] == "" {
		m.hint = agents[0]
	}
	return m, nil
}
