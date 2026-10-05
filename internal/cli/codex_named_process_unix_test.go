//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An untouched composer is a valid session state, not a naming failure.
// Exercise the real wait loop through idle probes and a verified process exit.
func TestCodexNamingWaitsForIdleComposerAndStopsWhenProcessEnds(t *testing.T) {
	oldLocate, oldPoll, oldStart := locateCodexProcessThreadForWait, codexNamedDiscoveryPoll, startCodexNamingSidecar
	t.Cleanup(func() {
		locateCodexProcessThreadForWait, codexNamedDiscoveryPoll, startCodexNamingSidecar = oldLocate, oldPoll, oldStart
	})
	codexNamedDiscoveryPoll = time.Millisecond
	calls := 0
	locateCodexProcessThreadForWait = func(ctx context.Context, _ codexNamingTarget) (codexProcessThread, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > codexNamedProbeTimeout {
			t.Fatal("discovery probe has no bounded deadline")
		}
		calls++
		if calls < 4 {
			return codexProcessThread{}, errCodexThreadNotReady
		}
		return codexProcessThread{}, errCodexNamingTargetEnded
	}
	startCodexNamingSidecar = func(string) (*codexSidecar, error) {
		t.Fatal("idle composer caused a naming mutation")
		return nil, errors.New("unexpected")
	}
	if err := runCodexNamedSidecar("session1/codex", codexNamingTarget{}); err != nil {
		t.Fatalf("normal idle-then-exit produced a warning: %v", err)
	}
	if calls != 4 {
		t.Fatalf("discovery calls = %d, want 4", calls)
	}
}

func TestValidateCodexNamingTargetRejectsReplacedProcess(t *testing.T) {
	oldInspect := inspectCodexNamingProcess
	t.Cleanup(func() { inspectCodexNamingProcess = oldInspect })
	inspectCodexNamingProcess = func(int) wakeProcessInfo {
		return wakeProcessInfo{Running: true, StartToken: "replacement", BootID: "boot"}
	}
	_, err := validateCodexNamingTarget(codexNamingTarget{PID: 42, ProcessStart: "original", BootID: "boot"})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("validateCodexNamingTarget error = %v, want replacement refusal", err)
	}
}

// Review of #950 (Pro r1 redesign): coop exec types /rename into the spawned
// Codex TUI only when Codex trusts the directory; at the trust prompt the
// typed keys would answer it.
func TestCodexTrustsDirReadsTheProjectTrustLevel(t *testing.T) {
	home, trusted, other := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	config := "[projects.\"" + trusted + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{trusted: true, other: false} {
		if got, err := codexTrustsDir(dir); err != nil || got != want {
			t.Fatalf("codexTrustsDir(%s) = %v, %v; want %v", dir, got, err, want)
		}
	}
}
