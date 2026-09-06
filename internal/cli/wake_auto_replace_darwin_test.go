//go:build darwin

package cli

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAcquireWakeLockRefusesDifferentLiveTerminalRawWakeWithoutSignal(t *testing.T) {
	const (
		wakePID         = 66121
		wakeTerminal    = 268435464
		currentTerminal = 268435465
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "different-live-terminal",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:                       pid,
			Running:                   true,
			StartToken:                "start",
			BootID:                    "boot",
			Executable:                "/opt/homebrew/bin/amq",
			Args:                      []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
			ControllingTerminalKnown:  true,
			HasControllingTerminal:    true,
			ControllingTerminalDevice: wakeTerminal,
		}
	})
	oldKinfo := readDarwinKinfoProc
	readDarwinKinfoProc = func(string, ...int) (*unix.KinfoProc, error) {
		return &unix.KinfoProc{
			Proc:  unix.ExternProc{P_stat: 1},
			Eproc: unix.Eproc{Tdev: currentTerminal},
		}, nil
	}
	t.Cleanup(func() { readDarwinKinfoProc = oldKinfo })
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("different-terminal wake must not be signaled without consent")
		return nil
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if cleanup != nil {
		cleanup()
		t.Fatal("different-terminal wake unexpectedly returned cleanup")
	}
	var alreadyRunning *wakeAlreadyRunningError
	if !errors.As(err, &alreadyRunning) {
		t.Fatalf("different-terminal acquisition error = %v, want wake already running", err)
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != "different-live-terminal" {
		t.Fatalf("different-terminal lock changed: %#v", current)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("different-terminal lock removed: %v", statErr)
	}
}
