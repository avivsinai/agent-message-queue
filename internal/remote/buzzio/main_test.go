package buzzio

import (
	"os"
	"testing"
)

// TestMain keeps the Claude settings file under the temp home each test
// passes: CLAUDE_CONFIG_DIR would move it to the developer's real one.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Exit(m.Run())
}
