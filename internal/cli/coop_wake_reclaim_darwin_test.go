//go:build darwin

package cli

import (
	"os"
	"syscall"
	"testing"
)

func TestPrepareCoopWakeLockLiveRawOrphanYesSignalsWaitsAndRemoves(t *testing.T) {
	const pid = 66121
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: pid, ProcessStart: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}, Generation: "raw"})
	stopped := false
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: got}
		}
		return wakeProcessInfo{PID: got, Running: true, StartToken: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}, ControllingTerminalKnown: true}
	})
	stubSignalWakeProcess(t, func(got int, signal os.Signal) error {
		if got != pid || signal != syscall.SIGTERM {
			t.Fatalf("signal %d %v", got, signal)
		}
		stopped = true
		return nil
	})
	oldGrace := wakeTerminateGrace
	wakeTerminateGrace = 0
	t.Cleanup(func() { wakeTerminateGrace = oldGrace })
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("retire raw orphan: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("raw orphan lock remains: %v", err)
	}
}
