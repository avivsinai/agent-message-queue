package kanban

import (
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
)

// TestMain drops an inherited AMQ session pin: delivery refuses a root the
// pin does not name, and each test delivers into its own temp root.
func TestMain(m *testing.M) {
	for _, name := range []string{sessionguard.EnvRoot, sessionguard.EnvBaseRoot, sessionguard.EnvSession, sessionguard.EnvRootID, sessionguard.EnvBaseRootID} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
