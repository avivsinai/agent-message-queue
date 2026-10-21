package adapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// isExecutableRegularFile applies the host's executable-file contract.
// Windows does not expose Unix execute permission bits through FileMode; a
// regular .exe selected by LookPath is executable by construction.
func isExecutableRegularFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

func platformExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// providerExecutable names the provider CLI a seat pins and the help output
// that proves the CLI has the subcommand the seat drives.
type providerExecutable struct {
	provider  string   // launch provider and PATH name, e.g. "codex"
	helpArgs  []string // argv that prints the help to check
	helpFlags []string // flags the help must mention
	lacks     string   // what the CLI lacks when a flag is missing
	hint      string   // remedy that ends every error
}

// pinExecutable resolves the provider CLI on PATH to an absolute,
// symlink-free executable regular file whose help shows every required flag.
func pinExecutable(ctx context.Context, lookPath func(string) (string, error), runner CommandRunner, spec providerExecutable) (string, error) {
	name, hint := spec.provider, spec.hint
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	looked, err := lookPath(platformExecutableName(name))
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH (%w); %s", name, err, hint)
	}
	if !filepath.IsAbs(looked) {
		abs, absErr := filepath.Abs(looked)
		if absErr != nil {
			return "", fmt.Errorf("resolve %s executable: %w; %s", name, absErr, hint)
		}
		looked = abs
	}
	// Mirror launch.ProviderForExecutable: identity is the PATH basename
	// before symlink evaluation so version-manager shims keep the provider name.
	if launch.ProviderForExecutable(looked) != spec.provider {
		return "", fmt.Errorf("%s at %s has basename %q, want %q; %s", name, looked, filepath.Base(looked), spec.provider, hint)
	}
	resolved, err := filepath.EvalSymlinks(looked)
	if err != nil {
		return "", fmt.Errorf("resolve %s at %s: %w; %s", name, looked, err, hint)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s at %s: %w; %s", name, resolved, err, hint)
	}
	if !isExecutableRegularFile(info) {
		return "", fmt.Errorf("%s at %s is not an executable regular file; %s", name, resolved, hint)
	}
	out, err := runner.Run(ctx, resolved, spec.helpArgs...)
	help := string(out)
	missing := err != nil
	for _, flag := range spec.helpFlags {
		missing = missing || !strings.Contains(help, flag)
	}
	if missing {
		return "", fmt.Errorf("%s at %s lacks %s; %s", name, resolved, spec.lacks, hint)
	}
	return resolved, nil
}
