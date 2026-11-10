package source

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// projectWorktrees scopes local records to canonical project roots. Outside
// Git, only the exact project directory is eligible. Other Git failures do
// not establish scope. A listed worktree whose directory is gone is skipped.
func projectWorktrees(ctx context.Context, env board.Env) (map[string]bool, error) {
	out, err := env.Run.Run(ctx, env.Dir, []string{"git", "worktree", "list", "--porcelain", "-z"})
	paths := []string{}
	if err != nil {
		if !outsideGit(err) {
			return nil, err
		}
		path, err := filepath.Abs(env.Dir)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	} else {
		for _, field := range strings.Split(string(out), "\x00") {
			if path, ok := strings.CutPrefix(field, "worktree "); ok {
				paths = append(paths, path)
			}
		}
	}
	worktrees := map[string]bool{}
	for _, path := range paths {
		real, err := filepath.EvalSymlinks(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		worktrees[real] = true
	}
	return worktrees, nil
}

// outsideGit reports whether a git error says the directory is in no
// repository. The runner runs git in the C locale, and keeps only the last
// stderr line, so the two forms are git's English messages for a search that
// reached the root or stopped at a filesystem boundary.
func outsideGit(err error) bool {
	msg := strings.TrimSpace(err.Error())
	return strings.HasPrefix(msg, "git: fatal: not a git repository (or any ") ||
		strings.HasPrefix(msg, "git: Stopping at filesystem boundary")
}
