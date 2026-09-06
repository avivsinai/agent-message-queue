//go:build darwin || linux

package cli

import (
	"os"
	"strings"
	"testing"
)

func TestRunOpsChecksReportsProvenStartMismatchAsStale(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "recorded-start",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "actual-start",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	result := runOpsChecks(root, "test", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) || got.Reason != "process start time mismatch" {
		t.Fatalf("unexpected wake lock: %#v", got)
	}
}

func TestRunOpsChecksFixRemovesProvenStartMismatch(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "recorded-start",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "actual-start",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	result := runOpsChecks(root, "test", true)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != "fixed" || !got.Removed {
		t.Fatalf("unexpected wake lock fix result: %#v", got)
	}
	if strings.Contains(got.NextAction, "remove the proven-stale lock") {
		t.Fatalf("fixed wake advertised stale-lock removal again: %#v", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("proven stale lock still exists: %v", err)
	}
}
