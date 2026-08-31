//go:build darwin || linux

package cli

import (
	"strings"
	"testing"
)

func TestWakeMutationScopeExpiresBeforeGuardRelease(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	var escaped *wakeMutationScope
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		escaped = scope
		if scope.guard == nil || scope.guard.file == nil {
			t.Fatal("mutation scope did not retain its lifecycle guard lease")
		}
		if _, _, err := scope.location(); err != nil {
			t.Fatalf("live mutation scope location = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if escaped == nil {
		t.Fatal("mutation scope was not retained for lifetime check")
	}
	if _, _, err := escaped.location(); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("escaped scope location error = %v, want inactive scope", err)
	}
	if err := escaped.unlinkWakeLock(); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("escaped scope unlink error = %v, want inactive scope", err)
	}
}
