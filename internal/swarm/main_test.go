package swarm

import (
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
)

// TestMain keeps the teams and tasks directories under the temp HOME each
// test sets: CLAUDE_CONFIG_DIR would move them to the developer's real one.
// It also drops an inherited AMQ session pin, which delivery checks against
// each test's temp root.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	for _, name := range []string{sessionguard.EnvRoot, sessionguard.EnvBaseRoot, sessionguard.EnvSession, sessionguard.EnvRootID, sessionguard.EnvBaseRootID} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
