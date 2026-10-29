package swarm

import (
	"os"
	"testing"
)

// TestMain keeps the teams and tasks directories under the temp HOME each
// test sets: CLAUDE_CONFIG_DIR would move them to the developer's real one.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Exit(m.Run())
}
