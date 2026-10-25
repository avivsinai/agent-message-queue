//go:build darwin || linux

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexLoginConfigured uses only the documented success status. The command
// owns auth storage; its output can contain account or masked-key details.
func codexLoginConfigured(binaryPath, wd, dir, codexHome string) bool {
	if !filepath.IsAbs(binaryPath) && strings.ContainsRune(binaryPath, filepath.Separator) {
		binaryPath = filepath.Join(wd, binaryPath)
	}
	if !filepath.IsAbs(codexHome) {
		codexHome = filepath.Join(wd, codexHome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "login", "status")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	return runHarnessProbe(ctx, cmd)
}
