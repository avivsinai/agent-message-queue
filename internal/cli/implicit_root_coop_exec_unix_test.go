//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCoopExecNamedSessionRejectsBrokenProjectAmqrcBeforeProvision(t *testing.T) {
	projectDir := enterBrokenRootProject(t)
	for _, key := range []string{envBaseRoot, envSession, envRootID, envBaseRootID} {
		setOptionalEnv(t, key, "", false)
	}
	execCalled := false
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error {
		execCalled = true
		return errors.New("unexpected exec")
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{
		"--session", "feature",
		"--me", "codex",
		"--no-wake",
		"sh",
	})

	requireBrokenAmqrcError(t, err)
	if execCalled {
		t.Fatal("coop exec reached process replacement after config refusal")
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("coop exec provisioned an implicit fallback before config refusal: %v", statErr)
	}
}

func TestCoopExecExplicitRootOverridesBrokenProjectAmqrc(t *testing.T) {
	enterBrokenRootProject(t)
	targetRoot := filepath.Join(t.TempDir(), "target-root")
	configureSendTestRoot(t, targetRoot, "codex")

	sentinel := errors.New("exec sentinel")
	var execEnv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string(nil), env...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{
		"--root", targetRoot,
		"--me", "codex",
		"--no-wake",
		"sh",
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want process sentinel", err)
	}
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, targetRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want explicit override %q", got, targetRoot)
	}
}
