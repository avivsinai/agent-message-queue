//go:build darwin

package cli

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAcquireWakeLockAutomaticallyReplacesTerminalGoneRawWake(t *testing.T) {
	testAcquireWakeLockAutomaticallyReplacesRawWake(t, wakeProcessInfo{
		ControllingTerminalKnown: true,
	})
}

func TestAcquireWakeLockAutomaticallyReplacesSameTerminalDifferentSessionRawWake(t *testing.T) {
	const (
		wakePID = 66121
		tdev    = 268435464
	)
	oldKinfo := readDarwinKinfoProc
	readDarwinKinfoProc = func(name string, args ...int) (*unix.KinfoProc, error) {
		if name != "kern.proc.pid" || len(args) != 1 || args[0] != os.Getpid() {
			t.Fatalf("unexpected current-process lookup: name=%q args=%v", name, args)
		}
		return &unix.KinfoProc{
			Proc:  unix.ExternProc{P_stat: 1},
			Eproc: unix.Eproc{Tdev: tdev},
		}, nil
	}
	t.Cleanup(func() { readDarwinKinfoProc = oldKinfo })
	stubWakeProcessSID(t, func(pid int) (int, error) {
		switch pid {
		case wakePID:
			return 100, nil
		case 0:
			return 200, nil
		default:
			t.Fatalf("unexpected SID lookup for pid %d", pid)
			return 0, nil
		}
	})

	testAcquireWakeLockAutomaticallyReplacesRawWake(t, wakeProcessInfo{
		ControllingTerminalKnown:  true,
		HasControllingTerminal:    true,
		ControllingTerminalDevice: tdev,
	})
}

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

func testAcquireWakeLockAutomaticallyReplacesRawWake(
	t *testing.T,
	terminal wakeProcessInfo,
) {
	t.Helper()

	const wakePID = 66121
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "automatic-replacement",
	})
	stopped := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		if stopped {
			return wakeProcessInfo{PID: pid}
		}
		terminal.PID = pid
		terminal.Running = true
		terminal.StartToken = "start"
		terminal.BootID = "boot"
		terminal.Executable = "/opt/homebrew/bin/amq"
		terminal.Args = []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}
		return terminal
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, signal os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		signals = append(signals, signal)
		if signal == syscall.SIGTERM {
			stopped = true
		}
		return nil
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("automatic raw-wake replacement: %v", err)
	}
	if cleanup == nil {
		t.Fatal("automatic raw-wake replacement returned nil cleanup")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want exact SIGTERM", signals)
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists ||
		current.Lock.PID != os.Getpid() ||
		current.Lock.Generation == "automatic-replacement" {
		t.Fatalf("replacement lock = %#v", current)
	}

	cleanup()
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("replacement cleanup left lock: %v", statErr)
	}
}
