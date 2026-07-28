package cli

import (
	"encoding/json"
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

func TestStandaloneImplicitRootCommandsAllowHigherPrecedenceOverrides(t *testing.T) {
	type rootSource struct {
		name string
		args func(string) []string
		env  func(*testing.T, string)
	}
	sources := []rootSource{
		{
			name: "explicit root",
			args: func(root string) []string { return []string{"--root", root} },
			env:  func(*testing.T, string) {},
		},
		{
			name: "AM_ROOT",
			args: func(string) []string { return nil },
			env: func(t *testing.T, root string) {
				t.Setenv(envRoot, root)
			},
		},
	}

	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			t.Run("init", func(t *testing.T) {
				enterBrokenRootProject(t)
				targetRoot := filepath.Join(t.TempDir(), "target-root")
				source.env(t, targetRoot)

				args := append(source.args(targetRoot), "--agents", "codex")
				if err := runInit(args); err != nil {
					t.Fatalf("init with override: %v", err)
				}
				if _, err := os.Stat(filepath.Join(targetRoot, "meta", "config.json")); err != nil {
					t.Fatalf("override root was not initialized: %v", err)
				}
			})

			t.Run("trace", func(t *testing.T) {
				enterBrokenRootProject(t)
				targetRoot := filepath.Join(t.TempDir(), "target-root")
				if err := os.MkdirAll(filepath.Join(targetRoot, "agents"), 0o700); err != nil {
					t.Fatal(err)
				}
				source.env(t, targetRoot)

				args := append([]string{"missing-message"}, source.args(targetRoot)...)
				args = append(args, "--json")
				stdout, _, err := captureEnvOutput(t, func() error {
					return runTrace(args)
				})
				if err == nil || GetExitCode(err) != ExitNotFound {
					t.Fatalf("trace error = %v, want not-found from override root", err)
				}
				var result traceResult
				if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
					t.Fatalf("decode trace output: %v\n%s", decodeErr, stdout)
				}
				if result.Root != targetRoot {
					t.Fatalf("trace root = %q, want override %q", result.Root, targetRoot)
				}
			})

			t.Run("kanban bridge", func(t *testing.T) {
				enterBrokenRootProject(t)
				targetRoot := filepath.Join(t.TempDir(), "missing-target")
				source.env(t, targetRoot)

				args := append(source.args(targetRoot), "--me", "codex")
				err := runKanbanBridge(args)
				if err == nil || !strings.Contains(err.Error(), targetRoot) || !strings.Contains(err.Error(), "does not exist") {
					t.Fatalf("kanban bridge error = %v, want override-root existence check", err)
				}
			})

			t.Run("symphony init", func(t *testing.T) {
				projectDir := enterBrokenRootProject(t)
				targetRoot := filepath.Join(t.TempDir(), "target-root")
				source.env(t, targetRoot)
				workflowPath := filepath.Join(projectDir, "WORKFLOW.md")
				if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o600); err != nil {
					t.Fatal(err)
				}

				args := append(source.args(targetRoot),
					"--workflow", workflowPath,
					"--me", "codex",
					"--check",
				)
				if err := runSymphonyInit(args); err != nil {
					t.Fatalf("symphony init with override: %v", err)
				}
			})

			t.Run("symphony emit", func(t *testing.T) {
				projectDir := enterBrokenRootProject(t)
				targetRoot := filepath.Join(t.TempDir(), "target-root")
				configureSendTestRoot(t, targetRoot, "codex")
				source.env(t, targetRoot)

				args := append(source.args(targetRoot),
					"--event", "after_create",
					"--me", "codex",
					"--workspace", projectDir,
				)
				if err := runSymphonyEmit(args); err != nil {
					t.Fatalf("symphony emit with override: %v", err)
				}
				if got := inboxCount(t, targetRoot, "codex"); got != 1 {
					t.Fatalf("override inbox count = %d, want 1", got)
				}
			})
		})
	}
}

func TestStandaloneImplicitRootCommandHelpIgnoresBrokenProjectAmqrc(t *testing.T) {
	enterBrokenRootProject(t)

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "init", run: func() error { return runInit([]string{"--help"}) }},
		{name: "trace", run: func() error { return runTrace([]string{"--help"}) }},
		{name: "kanban bridge", run: func() error { return runKanbanBridge([]string{"--help"}) }},
		{name: "symphony init", run: func() error { return runSymphonyInit([]string{"--help"}) }},
		{name: "symphony emit", run: func() error { return runSymphonyEmit([]string{"--help"}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := captureEnvOutput(t, test.run); err != nil {
				t.Fatalf("help with broken .amqrc: %v", err)
			}
		})
	}
}
