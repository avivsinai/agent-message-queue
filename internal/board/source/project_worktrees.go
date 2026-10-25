package source

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// projectWorktrees scopes local records to canonical project roots. Outside
// Git, only the exact project directory is eligible. Other Git failures do
// not establish scope.
func projectWorktrees(ctx context.Context, env board.Env) (map[string]bool, error) {
	out, err := env.Run.Run(ctx, env.Dir, []string{"git", "worktree", "list", "--porcelain", "-z"})
	paths := []string{}
	if err != nil {
		if strings.TrimSpace(err.Error()) != "git: fatal: not a git repository (or any of the parent directories): .git" {
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
		if err != nil {
			return nil, err
		}
		worktrees[real] = true
	}
	return worktrees, nil
}
