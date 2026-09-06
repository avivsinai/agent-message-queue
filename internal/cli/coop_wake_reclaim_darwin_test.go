//go:build darwin

package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrepareCoopWakeLockLiveRawAttachedIsRefusedWithoutMutation(t *testing.T) {
	testPrepareCoopWakeLockHealthyRawRefused(t, "/dev/null", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	})
}

func TestPrepareCoopWakeLockUnverifiedNeverSignals(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("unverified wake must never be signaled")
		return nil
	})

	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("prepare unverified wake: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("unverified metadata remains: %v", err)
	}
}

func testPrepareCoopWakeLockHealthyRawRefused(
	t *testing.T,
	tty string,
	terminal wakeProcessInfo,
) {
	t.Helper()

	const pid = 66121
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          tty,
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		Generation:   "healthy-live-raw",
	})
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		terminal.PID = got
		terminal.Running = true
		terminal.StartToken = "start"
		terminal.BootID = "boot"
		terminal.Executable = "/opt/homebrew/bin/amq"
		terminal.Args = []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}
		return terminal
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("healthy raw wake must not be signaled")
		return nil
	})

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return prepareCoopWakeLock(root, "codex", true, "unused")
	})
	if err == nil {
		t.Fatal("healthy raw wake in another terminal was accepted")
	}
	for _, want := range []string{
		"owned by a live process",
		"pid:     66121",
		"tty:     " + tty,
		"started: ",
		"use that terminal",
		"stop process 66121",
	} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("live conflict error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "doctor") {
		t.Fatalf("live conflict incorrectly recommends doctor: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("healthy raw wake emitted output: stdout=%q stderr=%q", stdout, stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("healthy raw lock changed: %v", err)
	}
}

func writeUnverifiedCoopWakeLock(t *testing.T, root string) string {
	t.Helper()
	return writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, TTY: "", Hostname: "definitely-not-this-host", Started: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339), Generation: "unverified"})
}
