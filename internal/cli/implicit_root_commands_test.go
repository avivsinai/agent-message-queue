package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enterBrokenRootProject(t *testing.T) string {
	t.Helper()

	fakeHome := t.TempDir()
	projectDir := filepath.Join(fakeHome, "workspace", "broken-project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write broken .amqrc: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	t.Setenv("HOME", fakeHome)
	for _, key := range []string{envRoot, envBaseRoot, envSession, envGlobalRoot} {
		t.Setenv(key, "")
	}
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

func requireBrokenAmqrcError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Fatalf("error = %v, want broken project .amqrc refusal", err)
	}
}

func TestStandaloneImplicitRootCommandsRejectBrokenProjectAmqrcBeforeSideEffects(t *testing.T) {
	t.Run("init before queue creation", func(t *testing.T) {
		projectDir := enterBrokenRootProject(t)

		err := runInit([]string{"--agents", "codex", "--force"})

		requireBrokenAmqrcError(t, err)
		if _, statErr := os.Stat(filepath.Join(projectDir, defaultCoopRoot)); !os.IsNotExist(statErr) {
			t.Fatalf("implicit fallback root was created: %v", statErr)
		}
	})

	t.Run("trace before evidence read or output", func(t *testing.T) {
		enterBrokenRootProject(t)

		stdout, _, err := captureEnvOutput(t, func() error {
			return runTrace([]string{"missing-message", "--json"})
		})

		requireBrokenAmqrcError(t, err)
		if stdout != "" {
			t.Fatalf("trace emitted output before config refusal: %q", stdout)
		}
	})

	t.Run("kanban bridge before root mutation or connection", func(t *testing.T) {
		projectDir := enterBrokenRootProject(t)

		err := runKanbanBridge([]string{"--me", "codex"})

		requireBrokenAmqrcError(t, err)
		if _, statErr := os.Stat(filepath.Join(projectDir, defaultCoopRoot)); !os.IsNotExist(statErr) {
			t.Fatalf("kanban bridge created implicit fallback root: %v", statErr)
		}
	})

	t.Run("symphony init before workflow rewrite", func(t *testing.T) {
		projectDir := enterBrokenRootProject(t)
		workflowPath := filepath.Join(projectDir, "WORKFLOW.md")
		const before = "# keep this workflow unchanged\n"
		if err := os.WriteFile(workflowPath, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}

		err := runSymphonyInit([]string{"--workflow", workflowPath, "--me", "codex"})

		requireBrokenAmqrcError(t, err)
		after, readErr := os.ReadFile(workflowPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != before {
			t.Fatalf("workflow changed before config refusal:\n%s", after)
		}
	})

	t.Run("symphony emit before delivery", func(t *testing.T) {
		projectDir := enterBrokenRootProject(t)
		fallbackRoot := filepath.Join(projectDir, defaultCoopRoot)
		configureSendTestRoot(t, fallbackRoot, "codex")

		err := runSymphonyEmit([]string{
			"--event", "after_create",
			"--me", "codex",
			"--workspace", projectDir,
		})

		requireBrokenAmqrcError(t, err)
		if got := inboxCount(t, fallbackRoot, "codex"); got != 0 {
			t.Fatalf("implicit fallback received %d symphony messages, want 0", got)
		}
	})
}
