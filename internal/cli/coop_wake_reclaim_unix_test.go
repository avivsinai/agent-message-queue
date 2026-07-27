//go:build darwin || linux

package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrepareCoopWakeLockRemovesProvenStaleWithoutPrompt(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, Generation: "stale"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })

	if err := prepareCoopWakeLock(root, "codex", false, "unused"); err != nil {
		t.Fatalf("prepare stale wake lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock remains: %v", err)
	}
}

func TestPrepareCoopWakeLockUnverifiedYesRemovesMetadataWithoutSignal(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	stderr := captureWakeStderr(t, func() {
		if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
			t.Fatalf("approved cleanup: %v", err)
		}
	})
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("unverified lock remains: %v", err)
	}
	if !strings.Contains(stderr, "without signaling it") || !strings.Contains(stderr, "duplicate notifications may continue") {
		t.Fatalf("warning missing safety facts: %q", stderr)
	}
}

func TestPrepareCoopWakeLockProvenForeignProcessRefusesWithoutMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, ProcessStart: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Generation: "foreign"})
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start", BootID: "boot", Executable: "/bin/sleep", Args: []string{"/bin/sleep", "100"}}
	})
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err == nil || !strings.Contains(err.Error(), "proven not to be this wake") {
		t.Fatalf("foreign process result = %v", err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("foreign process lock changed")
	}
}

func TestPrepareCoopWakeLockHeadlessPrintsRemedyWithoutPrompt(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	remedy := "amq coop exec -y --root /resolved/session --me codex codex"
	var got error
	stderr := captureWakeStderr(t, func() { got = prepareCoopWakeLock(root, "codex", false, remedy) })
	if got == nil || !strings.Contains(got.Error(), "declined") {
		t.Fatalf("headless result = %v", got)
	}
	if !strings.Contains(stderr, remedy) || !strings.Contains(stderr, "AM_ROOT=") {
		t.Fatalf("remedy missing: %q", stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("headless cleanup changed lock: %v", err)
	}
}

func TestCoopWakeRemedyForCommandUsesResolvedRootAndYes(t *testing.T) {
	got := coopWakeRemedyForCommand("/resolved/session", "codex", "codex", []string{"--dangerously-bypass-approvals-and-sandbox"})
	want := "amq coop exec -y --root /resolved/session --me codex codex --dangerously-bypass-approvals-and-sandbox"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func writeUnverifiedCoopWakeLock(t *testing.T, root string) string {
	t.Helper()
	return writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, TTY: "", Hostname: "definitely-not-this-host", Started: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339), Generation: "unverified"})
}
