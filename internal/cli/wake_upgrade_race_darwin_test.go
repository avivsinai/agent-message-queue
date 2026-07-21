//go:build darwin

package cli

import (
	"os"
	"strings"
	"testing"
)

func TestWakeUpgradeRetireLegacyWithoutControlMetadataFailsClosed(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("legacy inject-via retirement must not fall back to a bare PID signal")
		return nil
	})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "error" || !strings.Contains(result.Reason, "no cooperative control endpoint") {
		t.Fatalf("retire result = %#v err=%v, want missing-control failure", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("legacy lock was not preserved: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("legacy target was not preserved: %v", err)
	}
}
