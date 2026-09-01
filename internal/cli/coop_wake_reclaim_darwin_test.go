//go:build darwin

package cli

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrepareCoopWakeLockTerminalGoneDefersToSilentReplacement(t *testing.T) {
	const pid = 66121
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: pid, ProcessStart: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}, Generation: "raw"})
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		return wakeProcessInfo{PID: got, Running: true, StartToken: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}, ControllingTerminalKnown: true}
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("coop preflight must leave terminal-gone replacement to wake acquisition")
		return nil
	})

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return prepareCoopWakeLock(root, "codex", false, "unused")
	})
	if err != nil {
		t.Fatalf("prepare terminal-gone replacement: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("terminal-gone preflight output: stdout=%q stderr=%q", stdout, stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("preflight changed lock reserved for wake acquisition: %v", err)
	}
}

func TestPrepareCoopWakeLockLiveRawAttachedIsRefusedWithoutMutation(t *testing.T) {
	testPrepareCoopWakeLockHealthyRawRefused(t, "/dev/null", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	})
}

func TestPrepareCoopWakeLockLiveRawUnknownTTYAttachedIsRefusedWithoutMutation(t *testing.T) {
	testPrepareCoopWakeLockHealthyRawRefused(t, "unknown", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	})
}

func TestPrepareCoopWakeLockConsentedRawNumericSignalingFailsClosed(t *testing.T) {
	testPrepareCoopWakeLockLiveRawTakeoverRefused(t, "unknown", wakeProcessInfo{})
}

func TestPrepareCoopWakeLockSameTerminalDifferentSessionDefersToSilentReplacement(t *testing.T) {
	const (
		pid  = 66121
		tdev = 268435464
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		Generation:   "silent-replacement",
	})
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:                       got,
			Running:                   true,
			StartToken:                "start",
			BootID:                    "boot",
			Executable:                "/opt/homebrew/bin/amq",
			Args:                      []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
			ControllingTerminalKnown:  true,
			HasControllingTerminal:    true,
			ControllingTerminalDevice: tdev,
		}
	})
	oldKinfo := readDarwinKinfoProc
	readDarwinKinfoProc = func(string, ...int) (*unix.KinfoProc, error) {
		return &unix.KinfoProc{
			Proc:  unix.ExternProc{P_stat: 1},
			Eproc: unix.Eproc{Tdev: tdev},
		}, nil
	}
	t.Cleanup(func() { readDarwinKinfoProc = oldKinfo })
	stubWakeProcessSID(t, func(got int) (int, error) {
		if got == pid {
			return 100, nil
		}
		return 200, nil
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("coop preflight must leave silent replacement to wake acquisition")
		return nil
	})

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return prepareCoopWakeLock(root, "codex", false, "unused")
	})
	if err != nil {
		t.Fatalf("prepare silent replacement candidate: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("silent replacement preflight output: stdout=%q stderr=%q", stdout, stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("preflight changed lock reserved for wake acquisition: %v", err)
	}
}

func TestPrepareCoopWakeLockLiveGenericInjectViaRefusesWithoutMutation(t *testing.T) {
	const pid = 66121
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex", "--inject-via", "/bin/echo"},
		WakeMode:     wakeTargetInjectVia,
		Generation:   "inject-via",
	})
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        got,
			Running:    true,
			StartToken: "start",
			BootID:     "boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex", "--inject-via", "/bin/echo"},
		}
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("inject-via wake must not be signaled through coop takeover")
		return nil
	})

	err := prepareCoopWakeLock(root, "codex", true, "unused")
	if err == nil || !strings.Contains(err.Error(), "owned by a live process") {
		t.Fatalf("prepare live generic inject-via wake = %v, want live-owner refusal", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("inject-via lock changed: %v", err)
	}
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

func testPrepareCoopWakeLockLiveRawTakeoverRefused(
	t *testing.T,
	tty string,
	terminal wakeProcessInfo,
) {
	t.Helper()

	const pid = 66121
	root := secureTempDirForTest(t)
	establishDoctorWakeLifecycleGuardForTest(t, root, "codex")
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          tty,
		ProcessStart: "start",
		BootID:       "boot",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		Generation:   "live-raw-takeover",
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
		t.Fatal("darwin must not signal a raw wake by numeric PID")
		return nil
	})

	err := prepareCoopWakeLock(root, "codex", true, "unused")
	var operatorOnly *wakeOperatorOnlyError
	if !errors.As(err, &operatorOnly) || operatorOnly.RestartCapability() != wakeRestartOperatorOnly {
		t.Fatalf("consented live raw takeover error = %T %v, want typed operator_only", err, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("live raw lock changed: %v", statErr)
	}
}
