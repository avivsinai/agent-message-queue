//go:build darwin || linux

package cli

import (
	"errors"
	"path/filepath"
	"testing"
)

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
