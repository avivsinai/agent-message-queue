package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// TestMain runs the package in a home of its own: every turn reads
// ~/.amq/acp.settings, the hook check reads the Claude settings under the
// home, and a binding store with no path of its own is below ~/.amq. No
// test may read or write the developer's.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "amq-acp-home-")
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", binding.EnvPath} {
		_ = os.Unsetenv(name)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
