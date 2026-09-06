package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestEnvSessionFlagUsesPinnedCustomBaseFromForeignCWD(t *testing.T) {
	customBase := filepath.Join(t.TempDir(), "custom-queue")
	sourceRoot := filepath.Join(customBase, "session1")
	targetRoot := filepath.Join(customBase, "session2")
	for _, root := range []string{sourceRoot, targetRoot} {
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
		}
	}

	foreignProject := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreignProject, ".amqrc"), []byte(`{"root":"foreign-mail"}`), 0o600); err != nil {
		t.Fatalf("write foreign .amqrc: %v", err)
	}
	t.Chdir(foreignProject)

	t.Setenv(envRoot, sourceRoot)
	t.Setenv(envBaseRoot, customBase)
	t.Setenv(envSession, "session1")
	t.Setenv(envMe, "alice")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session2", "--me", "alice"})
	})
	if err != nil {
		t.Fatalf("runEnv --session: %v", err)
	}
	for _, want := range []string{
		"export AM_ROOT=" + shellQuotePosix(targetRoot) + "\n",
		"export AM_BASE_ROOT=" + shellQuotePosix(customBase) + "\n",
		"export AM_SESSION=session2\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("pinned-base session route missing %q: %q", want, stdout)
		}
	}
}

func setOptionalEnv(t *testing.T, key, value string, present bool) {
	t.Helper()
	old, hadOld := os.LookupEnv(key)
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	if present {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	} else if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}
