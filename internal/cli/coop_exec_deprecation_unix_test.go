//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestCoopExecHonorsDeclaredDefaultSessionSelectorFree(t *testing.T) {
	base := initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)
	writeProjectLaunchJSON(t, "dev")

	env, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	wantRoot := filepath.Join(base, "dev")
	if got := envValue(env, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("AM_ROOT = %q, want declared session %q", got, wantRoot)
	}
	if got := envValue(env, envSession); got != "dev" {
		t.Fatalf("AM_SESSION = %q, want dev", got)
	}
	requireDeprecationWarningCount(t, stderr, 1)
	if _, statErr := os.Stat(filepath.Join(wantRoot, "agents", "alice", "inbox", "new")); statErr != nil {
		t.Fatalf("declared session was not created: %v", statErr)
	}
}

func TestCoopExecExistingDeclaredDefaultSessionDoesNotWarn(t *testing.T) {
	base := initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)
	writeProjectLaunchJSON(t, "dev")
	if _, err := provisionCoopSession(base, "dev", []string{"alice"}, "", ""); err != nil {
		t.Fatalf("provision declared session: %v", err)
	}

	env, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	if got := envValue(env, envSession); got != "dev" {
		t.Fatalf("AM_SESSION = %q, want dev", got)
	}
	requireDeprecationWarningCount(t, stderr, 0)
}

func TestCoopExecExistingCollabSelectorFreeDoesNotWarn(t *testing.T) {
	initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)

	_, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	requireDeprecationWarningCount(t, stderr, 0)
}

func TestCoopExecMissingDeclaredCollabWarnsOnce(t *testing.T) {
	base := initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)
	if err := os.RemoveAll(filepath.Join(base, defaultSessionName)); err != nil {
		t.Fatalf("remove collab: %v", err)
	}

	_, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	requireDeprecationWarningCount(t, stderr, 1)
}

func TestCoopExecMissingExplicitSessionWarnsOnce(t *testing.T) {
	initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)

	env, stderr, err := captureCoopExecStderr(t, []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	if got := envValue(env, envSession); got != "feature" {
		t.Fatalf("AM_SESSION = %q, want feature", got)
	}
	requireDeprecationWarningCount(t, stderr, 1)
}

func TestCoopExecExistingExplicitSessionDoesNotWarn(t *testing.T) {
	base := initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)
	if _, err := provisionCoopSession(base, "feature", []string{"alice"}, "", ""); err != nil {
		t.Fatalf("provision feature: %v", err)
	}

	_, stderr, err := captureCoopExecStderr(t, []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	requireDeprecationWarningCount(t, stderr, 0)
}

func TestCoopExecMissingExplicitRootWarnsOnce(t *testing.T) {
	clearCoopSessionPinForTest(t)
	root := filepath.Join(t.TempDir(), "custom-root")

	env, stderr, err := captureCoopExecStderr(t, []string{"--root", root, "--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	if got := envValue(env, envRoot); !sameTreeIdentity(got, root) {
		t.Fatalf("AM_ROOT = %q, want %q", got, root)
	}
	requireDeprecationWarningCount(t, stderr, 1)
}

func TestCoopExecExistingExplicitRootDoesNotWarn(t *testing.T) {
	clearCoopSessionPinForTest(t)
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureCoopExecStderr(t, []string{"--root", root, "--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	requireDeprecationWarningCount(t, stderr, 0)
}

func TestCoopExecZeroConfigCollabBootstrapDoesNotWarn(t *testing.T) {
	enterFreshGitBootstrapProject(t, false)
	clearCoopSessionPinForTest(t)

	env, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "sh"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	if got := envValue(env, envSession); got != defaultSessionName {
		t.Fatalf("AM_SESSION = %q, want %q", got, defaultSessionName)
	}
	requireDeprecationWarningCount(t, stderr, 0)
}

func writeProjectLaunchJSON(t *testing.T, defaultSession string) {
	t.Helper()
	if err := os.MkdirAll(".amq", 0o700); err != nil {
		t.Fatalf("mkdir .amq: %v", err)
	}
	body, err := launch.MarshalProjectConfig(launch.ProjectConfig{
		Schema:         launch.ProjectConfigSchema,
		DefaultSession: defaultSession,
		Agents: []launch.ProjectAgentConfig{{
			Handle:       "claude",
			Adapter:      "claude",
			Command:      []string{"claude"},
			ResumePolicy: launch.ResumeEnabled,
		}},
		Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
	})
	if err != nil {
		t.Fatalf("marshal launch.json: %v", err)
	}
	if err := os.WriteFile(setupConfigPath, body, 0o600); err != nil {
		t.Fatalf("write launch.json: %v", err)
	}
}

func captureCoopExecStderr(t *testing.T, args []string) ([]string, string, error) {
	t.Helper()
	sentinel := errors.New("exec sentinel")
	var execEnv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string(nil), env...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopExec(args)
	})
	if !errors.Is(err, sentinel) {
		return execEnv, stderr, err
	}
	return execEnv, stderr, nil
}

func requireDeprecationWarningCount(t *testing.T, stderr string, want int) {
	t.Helper()
	got := strings.Count(stderr, coopExecCreationDeprecated)
	if got != want {
		t.Fatalf("deprecation warning count = %d, want %d\nstderr:\n%s", got, want, stderr)
	}
	if want == 0 {
		return
	}
	if strings.Contains(coopExecCreationDeprecated, "\n") {
		t.Fatal("deprecation warning constant must be a single line")
	}
}
