//go:build linux

package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrepareCoopWakeLockLiveRawUnknownTerminalYesSignalsAndRemoves(t *testing.T) {
	const (
		pid   = 4242
		pidfd = 99
		tty   = "unknown"
		state = "running; cannot determine whether its terminal is still attached"
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          tty,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "live-raw-unknown-terminal",
	})

	stopped := false
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID != pid {
			t.Fatalf("inspect pid = %d, want %d", gotPID, pid)
		}
		if stopped {
			return wakeProcessInfo{PID: gotPID}
		}
		return matchingLinuxWakeProcess(gotPID, root)
	})

	openCalls := 0
	signalCalls := 0
	pollCalls := 0
	stubLinuxPidfd(
		t,
		func(gotPID, flags int) (int, error) {
			openCalls++
			if gotPID != pid || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", gotPID, flags, pid)
			}
			return pidfd, nil
		},
		func(gotFD int, signal unix.Signal, _ *unix.Siginfo, flags int) error {
			signalCalls++
			if gotFD != pidfd || signal != unix.SIGTERM || flags != 0 {
				t.Fatalf("pidfd_send_signal = (%d, %v, %d), want (%d, SIGTERM, 0)", gotFD, signal, flags, pidfd)
			}
			return nil
		},
		func(gotFD int, timeout time.Duration) (bool, error) {
			pollCalls++
			if gotFD != pidfd {
				t.Fatalf("pidfd poll fd = %d, want %d", gotFD, pidfd)
			}
			if timeout <= 0 {
				t.Fatalf("pidfd poll timeout = %s, want positive", timeout)
			}
			stopped = true
			return true, nil
		},
	)

	var prepareErr error
	stderr := captureWakeStderr(t, func() {
		prepareErr = prepareCoopWakeLock(root, "codex", true, "unused")
	})
	if prepareErr != nil {
		t.Fatalf("approved live raw takeover: %v", prepareErr)
	}
	if openCalls != 1 {
		t.Errorf("pidfd open calls = %d, want 1", openCalls)
	}
	if signalCalls != 1 {
		t.Errorf("pidfd signal calls = %d, want 1 exact SIGTERM", signalCalls)
	}
	if pollCalls != 1 {
		t.Errorf("pidfd poll calls = %d, want 1", pollCalls)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("live raw lock remains after approved takeover: %v", err)
	}

	warningAt := strings.LastIndex(stderr, "warning:")
	if warningAt < 0 {
		t.Errorf("takeover warning missing from stderr: %q", stderr)
		return
	}
	warning := stderr[warningAt:]
	for _, want := range []string{"4242", tty, state} {
		if !strings.Contains(warning, want) {
			t.Errorf("takeover warning %q does not name %q", warning, want)
		}
	}
}

func TestPrepareCoopWakeLockLiveRawSelfCleanupAfterPidfdExitSucceeds(t *testing.T) {
	const (
		pid   = 4242
		pidfd = 99
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "live-raw-self-cleanup",
	})

	stopped := false
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID != pid {
			t.Fatalf("inspect pid = %d, want %d", gotPID, pid)
		}
		if stopped {
			return wakeProcessInfo{PID: gotPID}
		}
		return matchingLinuxWakeProcess(gotPID, root)
	})

	signalCalls := 0
	pollCalls := 0
	stubLinuxPidfd(
		t,
		func(gotPID, flags int) (int, error) {
			if gotPID != pid || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", gotPID, flags, pid)
			}
			return pidfd, nil
		},
		func(gotFD int, signal unix.Signal, _ *unix.Siginfo, flags int) error {
			signalCalls++
			if gotFD != pidfd || signal != unix.SIGTERM || flags != 0 {
				t.Fatalf("pidfd_send_signal = (%d, %v, %d), want (%d, SIGTERM, 0)", gotFD, signal, flags, pidfd)
			}
			return nil
		},
		func(gotFD int, timeout time.Duration) (bool, error) {
			pollCalls++
			if gotFD != pidfd {
				t.Fatalf("pidfd poll fd = %d, want %d", gotFD, pidfd)
			}
			if timeout <= 0 {
				t.Fatalf("pidfd poll timeout = %s, want positive", timeout)
			}
			stopped = true
			if err := os.Remove(lockPath); err != nil {
				t.Fatalf("simulate exact wake self-cleanup: %v", err)
			}
			return true, nil
		},
	)

	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Errorf("approved takeover after exact wake self-cleanup = %v, want success", err)
	}
	if signalCalls != 1 {
		t.Errorf("pidfd signal calls = %d, want 1 exact SIGTERM", signalCalls)
	}
	if pollCalls != 1 {
		t.Errorf("pidfd poll calls = %d, want 1 proven exit", pollCalls)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("self-cleaned live raw lock exists after successful takeover: %v", err)
	}
	guardInfo, err := os.Stat(wakeLifecycleGuardPath(root, "codex"))
	if err != nil {
		t.Fatalf("live wake cleanup did not retain lifecycle guard: %v", err)
	}
	if !guardInfo.Mode().IsRegular() || guardInfo.Mode().Perm() != 0o600 {
		t.Errorf("retained lifecycle guard mode = %v, want regular 0600", guardInfo.Mode())
	}
}

func TestPrepareCoopWakeLockLiveRawPartialTakeoverRefusesAfterLockDisappears(t *testing.T) {
	const (
		pid   = 4242
		pidfd = 99
		tty   = "unknown"
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          tty,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "live-raw-partial-takeover",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID != pid {
			t.Fatalf("inspect pid = %d, want %d", gotPID, pid)
		}
		// The lock mutation cannot be rolled back even though the exact helper
		// remains identity-same and live after the termination error.
		return matchingLinuxWakeProcess(gotPID, root)
	})

	var signals []unix.Signal
	pollCalls := 0
	processStillAliveAfterSIGTERM := false
	stubLinuxPidfd(
		t,
		func(gotPID, flags int) (int, error) {
			if gotPID != pid || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", gotPID, flags, pid)
			}
			return pidfd, nil
		},
		func(gotFD int, signal unix.Signal, _ *unix.Siginfo, flags int) error {
			if gotFD != pidfd || flags != 0 {
				t.Fatalf("pidfd_send_signal = (%d, %v, %d), want fd %d and flags 0", gotFD, signal, flags, pidfd)
			}
			signals = append(signals, signal)
			switch signal {
			case unix.SIGTERM:
				if err := os.Remove(lockPath); err != nil {
					t.Fatalf("simulate exact wake lock self-removal after SIGTERM: %v", err)
				}
				return nil
			case unix.SIGKILL:
				t.Fatal("must not send SIGKILL after wake lock disappearance")
				return nil
			default:
				t.Fatalf("unexpected wake signal: %v", signal)
				return nil
			}
		},
		func(gotFD int, timeout time.Duration) (bool, error) {
			pollCalls++
			if gotFD != pidfd {
				t.Fatalf("pidfd poll fd = %d, want %d", gotFD, pidfd)
			}
			if timeout <= 0 {
				t.Fatalf("pidfd poll timeout = %s, want positive", timeout)
			}
			if process := inspectWakeProcess(pid); !process.Running {
				t.Fatalf("pidfd poll fixture must keep wake process alive after SIGTERM: %+v", process)
			}
			processStillAliveAfterSIGTERM = true
			return false, nil
		},
	)

	var prepareErr error
	_ = captureWakeStderr(t, func() {
		prepareErr = prepareCoopWakeLock(root, "codex", true, "unused")
	})
	if prepareErr == nil || !strings.Contains(prepareErr.Error(), "wake lock is missing before SIGKILL") {
		t.Errorf("partially applied approved takeover = %v, want SIGKILL authorization refusal", prepareErr)
	}
	if len(signals) != 1 || signals[0] != unix.SIGTERM {
		t.Errorf("guarded pidfd signals = %v, want [SIGTERM]", signals)
	}
	if pollCalls != 1 {
		t.Errorf("pidfd poll calls = %d, want 1 before SIGKILL refusal", pollCalls)
	}
	if !processStillAliveAfterSIGTERM {
		t.Error("pidfd poll did not exercise a still-live wake process after SIGTERM")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("self-removed wake lock exists after partial takeover refusal: %v", err)
	}
	guardInfo, err := os.Stat(wakeLifecycleGuardPath(root, "codex"))
	if err != nil {
		t.Fatalf("live wake refusal did not retain lifecycle guard: %v", err)
	}
	if !guardInfo.Mode().IsRegular() || guardInfo.Mode().Perm() != 0o600 {
		t.Errorf("retained lifecycle guard mode = %v, want regular 0600", guardInfo.Mode())
	}
}

func TestTerminateRefusesNonLegacyWakeWhenLifecycleGuardIsAbsent(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex", "--inject-via", "/tmp/injector"},
		WakeMode:     wakeTargetInjectVia,
		Generation:   "non-legacy-without-guard",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingLinuxWakeProcess(pid, root)
	})

	inspection := inspectWakeLock(root, "codex")
	replaced, err := terminateAndRemoveOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "existing wake lifecycle guard") {
		t.Fatalf("non-legacy termination error = %v, want missing existing guard", err)
	}
	if replaced {
		t.Fatal("non-legacy wake was replaced without its lifecycle guard")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("non-legacy wake lock changed: %v", err)
	}
	if _, err := os.Stat(wakeLifecycleGuardPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("non-legacy refusal manufactured lifecycle guard: %v", err)
	}
}
