package source

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// codexGoalOwners reads project-scoped structural bindings through AMQ only.
// An ambiguous binding remains eligible but never claims an agent owner.
func codexGoalOwners(ctx context.Context, env board.Env) (map[string]board.Owner, map[string]bool, map[board.Owner]string, error) {
	out, err := env.Run.Run(ctx, env.Dir, []string{"amq", "session", "list", "--json"})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("codex goal mapping: %w", err)
	}
	var listing struct {
		Sessions []struct {
			Name          string                                         `json:"name"`
			Conversations []struct{ Handle, Provider, ID, State string } `json:"conversations"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return nil, nil, nil, fmt.Errorf("codex goal mapping: %w", err)
	}
	owners, known := map[string]board.Owner{}, map[string]bool{}
	kinds := map[board.Owner]string{}
	for _, session := range listing.Sessions {
		for _, c := range session.Conversations {
			if c.State != "ready" || fsq.ValidateHandle(c.Handle) != nil {
				continue
			}
			if session.Name != "" && fsq.ValidateHandle(session.Name) != nil {
				continue
			}
			candidate := board.Owner{Kind: board.OwnerAgent, Session: session.Name, Handle: c.Handle, Display: c.Handle}
			kinds[candidate] = c.Provider
			if c.Provider != "codex" {
				continue
			}
			if _, ok := board.CodexGoal(c.ID); !ok {
				continue
			}
			o := board.Owner{Kind: board.OwnerAgent, Session: session.Name, Handle: c.Handle, Display: c.Handle}
			if old, exists := owners[c.ID]; exists && old != o {
				o = board.Owner{Kind: board.OwnerUnmapped, Display: "ambiguous Codex session " + c.ID}
			}
			owners[c.ID], known[c.ID] = o, true
		}
	}
	return owners, known, kinds, nil
}

func codexGoalSessions(ctx context.Context, env board.Env, home string, owners map[string]board.Owner, known map[string]bool, kinds map[board.Owner]string) (map[string]codexGoalSession, board.Coverage, error) {
	eligible := map[string]codexGoalSession{}
	coverage := board.Coverage{}
	out, err := env.Run.Run(ctx, env.Dir, []string{"git", "worktree", "list", "--porcelain", "-z"})
	if err != nil {
		return nil, coverage, fmt.Errorf("codex goal project worktrees: %w", err)
	}
	worktrees := map[string]bool{}
	for _, field := range strings.Split(string(out), "\x00") {
		if p, ok := strings.CutPrefix(field, "worktree "); ok {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				worktrees[real] = true
			}
		}
	}
	// A ready structural binding is itself project eligibility. It remains
	// usable even when the rollout has aged out, moved, or been removed.
	for id := range known {
		o := owners[id]
		eligible[id] = codexGoalSession{Owner: o, Structural: o.Kind == board.OwnerAgent}
	}
	cutoff := env.Now().Add(-board.DefaultConfig().StaleAfter)
	err = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		name := entry.Name()
		if len(name) < 42 {
			return nil
		}
		id := name[len(name)-42 : len(name)-6]
		if _, ok := board.CodexGoal(id); !ok {
			return nil
		}
		if known[id] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			coverage.Skipped++
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		// Read only the first complete line. The rest is transcript data and
		// is never opened by this reader's decoder.
		line, readErr := bufio.NewReader(io.LimitReader(f, 1<<20)).ReadBytes('\n')
		_ = f.Close()
		if readErr != nil {
			coverage.Unresolved++
			return nil
		}
		var meta struct {
			Type    string                   `json:"type"`
			Payload struct{ ID, Cwd string } `json:"payload"`
		}
		if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != id {
			coverage.Unresolved++
			return nil
		}
		cwd, err := filepath.EvalSymlinks(meta.Payload.Cwd)
		if err != nil || !worktrees[cwd] {
			return nil
		}
		eligible[id] = codexGoalSession{Owner: board.Owner{Kind: board.OwnerUnmapped, Display: "codex session " + id + " in " + cwd}}
		return nil
	})
	if err == nil {
		hint, ok, hintErr := codexGoalOwnerHint(ctx, env, kinds)
		if hintErr != nil {
			return eligible, coverage, hintErr
		}
		if ok {
			for id, sess := range eligible {
				if !known[id] {
					sess.Owner = hint
					eligible[id] = sess
				}
			}
		}
	}
	if coverage.Skipped > 0 || coverage.Unresolved > 0 {
		coverage.Detail = fmt.Sprintf("%d inactive rollouts outside the 36h discovery window; %d incomplete or unreadable session headers", coverage.Skipped, coverage.Unresolved)
	}
	return eligible, coverage, err
}

// A cwd join is inferred only with one live candidate of the right harness,
// or one live agent total when AMQ has no harness fact for that agent.
func codexGoalOwnerHint(ctx context.Context, env board.Env, kinds map[board.Owner]string) (board.Owner, bool, error) {
	out, err := env.Run.Run(ctx, env.Dir, []string{"amq", "who", "--json"})
	if err != nil {
		return board.Owner{}, false, fmt.Errorf("codex goal owner hints: %w", err)
	}
	rows, err := parseWho(out)
	if err != nil {
		return board.Owner{}, false, fmt.Errorf("codex goal owner hints: %w", err)
	}
	var live, codex []board.Owner
	for key, agent := range rows {
		if !agent.Active || key[1] == reservedHuman || fsq.ValidateHandle(key[1]) != nil {
			continue
		}
		owner := board.Owner{Kind: board.OwnerAgent, Session: key[0], Handle: key[1], Display: key[1]}
		live = append(live, owner)
		if kinds[owner] == "codex" {
			codex = append(codex, owner)
		}
	}
	if len(codex) == 1 {
		return codex[0], true, nil
	}
	if len(codex) == 0 && len(live) == 1 && kinds[live[0]] == "" {
		return live[0], true, nil
	}
	return board.Owner{}, false, nil
}
