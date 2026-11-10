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
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// codexGoalOwners reads project-scoped structural bindings through AMQ only.
// An ambiguous binding remains eligible but never claims an agent owner.
func codexGoalOwners(ctx context.Context, env board.Env) (map[string]board.Owner, map[string]bool, error) {
	out, err := env.Run.Run(ctx, env.Dir, []string{"amq", "session", "list", "--json"})
	if err != nil {
		return nil, nil, fmt.Errorf("codex goal mapping: %w", err)
	}
	var listing struct {
		Sessions []struct {
			Name          string                                         `json:"name"`
			Conversations []struct{ Handle, Provider, ID, State string } `json:"conversations"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return nil, nil, fmt.Errorf("codex goal mapping: %w", err)
	}
	owners, known := map[string]board.Owner{}, map[string]bool{}
	for _, session := range listing.Sessions {
		for _, c := range session.Conversations {
			if c.State != "ready" || fsq.ValidateHandle(c.Handle) != nil {
				continue
			}
			if session.Name != "" && fsq.ValidateHandle(session.Name) != nil {
				continue
			}
			if c.Provider != "codex" {
				continue
			}
			if !board.ValidHarnessID(c.ID) {
				continue
			}
			o := board.Owner{Kind: board.OwnerAgent, Session: session.Name, Handle: c.Handle, Display: c.Handle}
			if old, exists := owners[c.ID]; exists && old != o {
				o = board.Owner{Kind: board.OwnerUnmapped, Display: "ambiguous Codex session " + c.ID}
			}
			owners[c.ID], known[c.ID] = o, true
		}
	}
	return owners, known, nil
}

func codexGoalSessions(ctx context.Context, env board.Env, home string, owners map[string]board.Owner, known map[string]bool) (map[string]codexGoalSession, board.Coverage, error) {
	eligible := map[string]codexGoalSession{}
	coverage := board.Coverage{}
	worktrees, err := projectWorktrees(ctx, env)
	if err != nil {
		return nil, coverage, fmt.Errorf("codex goal project worktrees: %w", err)
	}
	// A ready structural binding is itself project eligibility. It remains
	// usable even when the rollout has aged out, moved, or been removed.
	for id := range known {
		o := owners[id]
		eligible[id] = codexGoalSession{Owner: o, Structural: o.Kind == board.OwnerAgent}
	}
	var skipped []string
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
		if !board.ValidHarnessID(id) {
			return nil
		}
		if known[id] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				skipped = append(skipped, name)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fileCwd, ok := rolloutCwd(path, id, info)
		if !ok {
			skipped = append(skipped, name)
			return nil
		}
		cwd, err := filepath.EvalSymlinks(fileCwd)
		if err != nil || !worktrees[cwd] {
			return nil
		}
		eligible[id] = codexGoalSession{Owner: board.Owner{Kind: board.OwnerUnmapped, Display: "codex session " + id + " in " + cwd}}
		return nil
	})

	coverage.Unresolved = len(skipped)
	if len(skipped) > 0 {
		names := skipped
		if len(names) > 3 {
			names = names[:3]
		}
		coverage.Detail = fmt.Sprintf("%d incomplete or unreadable session headers: %s", len(skipped), strings.Join(names, ", "))
	}
	return eligible, coverage, err
}

// rolloutMemo keeps the working directory of a rollout header, keyed by path
// and validated by size and mtime, so an unchanged transcript is not opened
// again on the next read.
var rolloutMemo = struct {
	sync.Mutex
	m map[string]rolloutMemoEntry
}{m: map[string]rolloutMemoEntry{}}

type rolloutMemoEntry struct {
	size int64
	mod  time.Time
	cwd  string
}

// rolloutCwd reads only the first complete line of a rollout; the rest is
// transcript data. It reports false for a header that is unreadable or does
// not name id.
func rolloutCwd(path, id string, info fs.FileInfo) (string, bool) {
	rolloutMemo.Lock()
	hit, ok := rolloutMemo.m[path]
	rolloutMemo.Unlock()
	if ok && hit.size == info.Size() && hit.mod.Equal(info.ModTime()) {
		return hit.cwd, true
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	line, readErr := bufio.NewReader(io.LimitReader(f, 1<<20)).ReadBytes('\n')
	_ = f.Close()
	if readErr != nil {
		return "", false
	}
	var meta struct {
		Type    string                   `json:"type"`
		Payload struct{ ID, Cwd string } `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != id {
		return "", false
	}
	rolloutMemo.Lock()
	if len(rolloutMemo.m) >= 8192 {
		clear(rolloutMemo.m)
	}
	rolloutMemo.m[path] = rolloutMemoEntry{size: info.Size(), mod: info.ModTime(), cwd: meta.Payload.Cwd}
	rolloutMemo.Unlock()
	return meta.Payload.Cwd, true
}
