//go:build darwin || linux

package cli

import (
	"errors"
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
	if got := strings.Count(stderr, "duplicate notifications may continue"); got != 1 {
		t.Fatalf("duplicate-notification warning count = %d, want 1: %q", got, stderr)
	}
}

func TestResolveMissingWakeLockAfterTerminationPreservesPresentGenerationError(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        66121,
		Generation: "same-generation",
	})
	inspection := inspectWakeLock(root, "codex")
	terminationErr := errors.New("test termination failure")

	retired, err := resolveMissingWakeLockAfterTermination(inspection, terminationErr)
	if retired {
		t.Fatal("present exact generation reported retired")
	}
	if !errors.Is(err, terminationErr) {
		t.Fatalf("present exact generation error = %v, want %v", err, terminationErr)
	}
}

func TestResolveMissingWakeLockAfterTerminationReturnsRetryForReplacementGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        66121,
		Generation: "old-generation",
	})
	inspection := inspectWakeLock(root, "codex")
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        77121,
		Generation: "replacement-generation",
	})

	retired, err := resolveMissingWakeLockAfterTermination(
		inspection,
		errors.New("test termination failure"),
	)
	if err != nil {
		t.Fatalf("replacement-generation result = %v, want caller retry", err)
	}
	if retired {
		t.Fatal("replacement generation reported retired")
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != "replacement-generation" {
		t.Fatalf("replacement lock changed at %s: %#v", lockPath, current)
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
	stdout, stderr, got := captureEnvOutput(t, func() error {
		return prepareCoopWakeLock(root, "codex", false, remedy)
	})
	if got == nil || !strings.Contains(got.Error(), "declined") {
		t.Fatalf("headless result = %v", got)
	}
	if strings.Contains(stdout, "Clear it and start a fresh wake?") ||
		strings.Contains(stderr, "Clear it and start a fresh wake?") {
		t.Fatalf("headless cleanup printed an unanswerable prompt: stdout=%q stderr=%q", stdout, stderr)
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

func TestWakeLockHasUsableNotificationPathChoosesExplicitTerminalFailDirection(t *testing.T) {
	tests := []struct {
		name       string
		tty        string
		known      bool
		has        bool
		wantUsable bool
	}{
		{name: "attached legacy unknown is usable", tty: "unknown", known: true, has: true, wantUsable: true},
		{name: "undeterminable legacy unknown fails closed", tty: "unknown"},
		{name: "undeterminable concrete path preserves Linux evidence", tty: "/dev/null", wantUsable: true},
		{name: "proven gone fails closed", tty: "/dev/amq-missing-notification-tty", known: true, has: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspection := wakeLockInspection{
				Lock: wakeLock{TTY: tt.tty, WakeMode: wakeInjectModeRaw},
				Process: wakeProcessInfo{
					ControllingTerminalKnown: tt.known,
					HasControllingTerminal:   tt.has,
				},
			}
			if got := wakeLockHasUsableNotificationPath(inspection); got != tt.wantUsable {
				t.Fatalf("usable = %v, want %v", got, tt.wantUsable)
			}
		})
	}
}

func writeUnverifiedCoopWakeLock(t *testing.T, root string) string {
	t.Helper()
	return writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, TTY: "", Hostname: "definitely-not-this-host", Started: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339), Generation: "unverified"})
}
