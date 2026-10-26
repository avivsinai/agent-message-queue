//go:build !darwin && !linux

package cli

import (
	"context"
	"os/exec"
)

func codexLoginConfigured(_ context.Context, _, _, _, _ string) bool { return false }

// codexFeaturesListOutput runs `codex features list` in dir within ctx.
func codexFeaturesListOutput(ctx context.Context, cmdName, dir string) ([]byte, error) {
	list := exec.CommandContext(ctx, cmdName, "features", "list")
	list.Dir = dir
	return list.Output()
}
