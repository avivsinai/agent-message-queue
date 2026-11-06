package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain runs the package in a home of its own: the settings writer
// takes its locks and records the owner's opt-in under ~/.amq, and
// CLAUDE_CONFIG_DIR would move the settings file to the developer's real
// one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "amq-claude-home-")
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
