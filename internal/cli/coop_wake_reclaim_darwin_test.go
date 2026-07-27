//go:build darwin

package cli

import (
	"errors"
	"os"
	"strings"
	"syscall"
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

func TestPrepareCoopWakeLockLiveRawAttachedYesTakesOverWithWarning(t *testing.T) {
	testPrepareCoopWakeLockLiveRawTakeover(t, "/dev/null", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	}, "running, and still attached to a terminal — this may be a live session in another window", false)
}

func TestPrepareCoopWakeLockLiveRawUnknownTTYAttachedYesTakesOverWithWarning(t *testing.T) {
	testPrepareCoopWakeLockLiveRawTakeover(t, "unknown", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	}, "running, and still attached to a terminal — this may be a live session in another window", false)
}

func TestPrepareCoopWakeLockLiveRawUndeterminableYesTakesOverWithWarning(t *testing.T) {
	testPrepareCoopWakeLockLiveRawTakeover(t, "unknown", wakeProcessInfo{},
		"running; cannot determine whether its terminal is still attached", false)
}

func TestPrepareCoopWakeLockConsentedWakeSelfCleanupIsSuccess(t *testing.T) {
	testPrepareCoopWakeLockLiveRawTakeover(t, "/dev/null", wakeProcessInfo{
		ControllingTerminalKnown: true,
		HasControllingTerminal:   true,
	}, "running, and still attached to a terminal — this may be a live session in another window", true)
}

func TestPrepareCoopWakeLockConsentedWakeSelfCleanupWithoutProvenExitWarnsAndProceeds(t *testing.T) {
	tests := []struct {
		name        string
		afterSignal wakeProcessInfo
	}{
		{
			name: "same identity",
			afterSignal: wakeProcessInfo{
				Running:    true,
				StartToken: "start",
				BootID:     "boot",
				Executable: "/opt/homebrew/bin/amq",
			},
		},
		{
			name: "unknown identity",
			afterSignal: wakeProcessInfo{
				Running:      true,
				InspectError: errors.New("test identity inspection failure"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const (
				pid = 66121
				tty = "/dev/null"
			)
			root := secureTempDirForTest(t)
			args := []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}
			lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:          pid,
				TTY:          tty,
				ProcessStart: "start",
				BootID:       "boot",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         args,
				Generation:   "live-raw-partial-takeover",
			})
			signaled := false
			stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
				if signaled {
					info := test.afterSignal
					info.PID = got
					return info
				}
				return wakeProcessInfo{
					PID:                      got,
					Running:                  true,
					StartToken:               "start",
					BootID:                   "boot",
					Executable:               "/opt/homebrew/bin/amq",
					Args:                     args,
					ControllingTerminalKnown: true,
					HasControllingTerminal:   true,
				}
			})
			var signals []os.Signal
			stubSignalWakeProcess(t, func(got int, signal os.Signal) error {
				signals = append(signals, signal)
				if got != pid || signal != syscall.SIGTERM {
					t.Fatalf("signal = (%d, %v), want (%d, SIGTERM)", got, signal, pid)
				}
				signaled = true
				if err := os.Remove(lockPath); err != nil {
					t.Fatalf("simulate old wake self-cleanup: %v", err)
				}
				return nil
			})

			var prepareErr error
			stderr := captureWakeStderr(t, func() {
				prepareErr = prepareCoopWakeLock(root, "codex", true, "unused")
			})
			if prepareErr != nil {
				t.Fatalf("approved takeover after partial old-wake cleanup: %v", prepareErr)
			}
			if len(signals) != 1 || signals[0] != syscall.SIGTERM {
				t.Fatalf("signals = %v, want [SIGTERM]", signals)
			}
			if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
				t.Fatalf("self-cleaned live raw lock exists after takeover: %v", err)
			}
			for _, want := range []string{
				"superseded wake helper",
				"pid 66121",
				tty,
				"without a confirmed exit",
				"duplicate notifications may continue",
			} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("partial-takeover output %q missing %q", stderr, want)
				}
			}
			if got := strings.Count(stderr, "duplicate notifications may continue"); got != 1 {
				t.Fatalf("duplicate warning count = %d, want 1 in %q", got, stderr)
			}
		})
	}
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

func TestPrepareCoopWakeLockInjectViaNeverSignalsThroughTakeover(t *testing.T) {
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

	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("prepare inject-via wake: %v", err)
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

func testPrepareCoopWakeLockLiveRawTakeover(
	t *testing.T,
	tty string,
	terminal wakeProcessInfo,
	wantState string,
	selfRemoves bool,
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
		Generation:   "live-raw-takeover",
	})
	stopped := false
	stubInspectWakeProcess(t, func(got int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: got}
		}
		terminal.PID = got
		terminal.Running = true
		terminal.StartToken = "start"
		terminal.BootID = "boot"
		terminal.Executable = "/opt/homebrew/bin/amq"
		terminal.Args = []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}
		return terminal
	})
	stubSignalWakeProcess(t, func(got int, signal os.Signal) error {
		if got != pid || signal != syscall.SIGTERM {
			t.Fatalf("signal %d %v", got, signal)
		}
		stopped = true
		if selfRemoves {
			if err := os.Remove(lockPath); err != nil {
				t.Fatalf("old wake self-remove lock: %v", err)
			}
		}
		return nil
	})

	stderr := captureWakeStderr(t, func() {
		if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
			t.Fatalf("take over live raw wake: %v", err)
		}
	})
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("live raw lock remains: %v", err)
	}
	for _, want := range []string{
		wantState,
		"warning: took over blocking wake for codex",
		"pid 66121",
		"on " + tty,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("takeover output %q missing %q", stderr, want)
		}
	}
}
