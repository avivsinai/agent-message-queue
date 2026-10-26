//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexLoginConfigured uses only the documented success status. The command
// owns auth storage; its output can contain account or masked-key details.
func codexLoginConfigured(parent context.Context, binaryPath, wd, dir, codexHome string) bool {
	if !filepath.IsAbs(binaryPath) && strings.ContainsRune(binaryPath, filepath.Separator) {
		binaryPath = filepath.Join(wd, binaryPath)
	}
	if !filepath.IsAbs(codexHome) {
		codexHome = filepath.Join(wd, codexHome)
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "login", "status")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	return runHarnessProbe(ctx, cmd)
}

// codexFeaturesListOutput runs `codex features list` in dir within ctx. The
// npm launcher starts the native binary with inherited streams, so the
// process group is killed at the deadline and the output is bounded.
func codexFeaturesListOutput(ctx context.Context, cmdName, dir string) ([]byte, error) {
	list := exec.CommandContext(ctx, cmdName, "features", "list")
	list.Dir = dir
	output := boundedOutput{limit: 1 << 20}
	list.Stdin, list.Stdout, list.Stderr = nil, &output, io.Discard
	if !runHarnessProbe(ctx, list) {
		return nil, errors.New("did not complete")
	}
	return output.Bytes(), nil
}
