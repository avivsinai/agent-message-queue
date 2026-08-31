//go:build linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func stubWakeRestartPidfdForTest(
	t *testing.T,
	open func(int, int) (int, error),
	send func(int, unix.Signal, *unix.Siginfo, int) error,
	close func(int) error,
) {
	t.Helper()
	oldOpen := linuxPidfdOpen
	oldSend := linuxPidfdSend
	oldClose := linuxPidfdClose
	linuxPidfdOpen = open
	linuxPidfdSend = send
	linuxPidfdClose = close
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdSend = oldSend
		linuxPidfdClose = oldClose
	})
}

func TestLinuxWakeRestartAdvertisementUsesOnlySIGUSR1(t *testing.T) {
	lock := wakeLock{ControlSocket: "/tmp/stale-control.sock"}
	configureWakeRestartAdvertisementPlatform(&lock, "/ignored", "codex")
	if lock.ResumeSignal != wakeResumeSignalUSR1 || lock.ControlSocket != "" {
		t.Fatalf("restart transport = signal %q socket %q", lock.ResumeSignal, lock.ControlSocket)
	}
	if err := validateWakeRestartTransportPlatform(lock, "/ignored", "codex"); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		lock wakeLock
	}{
		{name: "missing_signal", lock: wakeLock{}},
		{name: "control_socket", lock: wakeLock{ResumeSignal: wakeResumeSignalUSR1, ControlSocket: "/tmp/control.sock"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateWakeRestartTransportPlatform(test.lock, "/ignored", "codex"); err == nil {
				t.Fatal("unsafe Linux restart transport was accepted")
			}
		})
	}
}

func TestNotifyWakeRestartLinuxUsesPidfdAfterExactLockRead(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	events := make([]string, 0, 4)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		events = append(events, "inspect")
		if pid == fixture.process.PID {
			return fixture.process
		}
		return wakeProcessInfo{PID: pid}
	})
	stubWakeRestartPidfdForTest(
		t,
		func(pid, flags int) (int, error) {
			events = append(events, "open")
			if pid != fixture.lock.PID || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", pid, flags, fixture.lock.PID)
			}
			return 41, nil
		},
		func(fd int, signal unix.Signal, info *unix.Siginfo, flags int) error {
			events = append(events, "send")
			if fd != 41 || signal != unix.SIGUSR1 || info != nil || flags != 0 {
				t.Fatalf("pidfd_send_signal = (%d, %v, %v, %d)", fd, signal, info, flags)
			}
			return nil
		},
		func(fd int) error {
			events = append(events, "close")
			if fd != 41 {
				t.Fatalf("close fd = %d, want 41", fd)
			}
			return nil
		},
	)

	if err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record); err != nil {
		t.Fatal(err)
	}
	if want := []string{"open", "inspect", "send", "close"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("call order = %v, want %v", events, want)
	}
}

func TestNotifyWakeRestartLinuxRefusesLockChangeBeforePidfdOpen(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	changed := fixture.lock.Lock
	changed.TTY = "replacement-tty"
	writeWakeLockForTest(t, fixture.root, fixture.agent, changed)
	stubWakeRestartPidfdForTest(
		t,
		func(int, int) (int, error) {
			t.Fatal("pidfd_open called for a changed wake lock")
			return -1, nil
		},
		func(int, unix.Signal, *unix.Siginfo, int) error {
			t.Fatal("pidfd_send_signal called for a changed wake lock")
			return nil
		},
		func(int) error {
			t.Fatal("close called without an acquired pidfd")
			return nil
		},
	)

	err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record)
	if err == nil || !strings.Contains(err.Error(), "changed before restart pidfd acquisition") {
		t.Fatalf("error = %v, want exact-lock refusal", err)
	}
}

func TestNotifyWakeRestartLinuxRefusesPidfdOpenErrors(t *testing.T) {
	for _, openErr := range []error{syscall.ENOSYS, syscall.EPERM, syscall.ESRCH} {
		t.Run(openErr.Error(), func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			closed := false
			stubWakeRestartPidfdForTest(
				t,
				func(int, int) (int, error) { return -1, openErr },
				func(int, unix.Signal, *unix.Siginfo, int) error {
					t.Fatal("pidfd_send_signal called after pidfd_open failure")
					return nil
				},
				func(int) error {
					closed = true
					return nil
				},
			)

			err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record)
			if !errors.Is(err, openErr) || !strings.Contains(err.Error(), "pidfd_open") {
				t.Fatalf("error = %v, want wrapped %v", err, openErr)
			}
			if closed {
				t.Fatal("close called without an acquired pidfd")
			}
		})
	}
}

func TestNotifyWakeRestartLinuxClosesPidfdOnSignalError(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	closed := false
	stubWakeRestartPidfdForTest(
		t,
		func(int, int) (int, error) { return 42, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { return syscall.ESRCH },
		func(fd int) error {
			closed = true
			if fd != 42 {
				t.Fatalf("close fd = %d, want 42", fd)
			}
			return nil
		},
	)

	err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record)
	if !errors.Is(err, syscall.ESRCH) || !strings.Contains(err.Error(), "pidfd_send_signal") {
		t.Fatalf("error = %v, want wrapped ESRCH", err)
	}
	if !closed {
		t.Fatal("pidfd was not closed after signal failure")
	}
}

func TestNotifyWakeRestartLinuxRefusesIdentityChangeAfterPidfdOpen(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	pidfdOpened := false
	signaled := false
	closed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != fixture.process.PID {
			return wakeProcessInfo{PID: pid}
		}
		process := fixture.process
		if pidfdOpened {
			process.StartToken += "-reused"
		}
		return process
	})
	stubWakeRestartPidfdForTest(
		t,
		func(int, int) (int, error) {
			pidfdOpened = true
			return 43, nil
		},
		func(int, unix.Signal, *unix.Siginfo, int) error {
			signaled = true
			return nil
		},
		func(int) error {
			closed = true
			return nil
		},
	)

	err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record)
	if err == nil || !strings.Contains(err.Error(), "wake changed before restart signal") {
		t.Fatalf("error = %v, want identity-change refusal", err)
	}
	if signaled {
		t.Fatal("identity-changed process was signaled")
	}
	if !closed {
		t.Fatal("pidfd was not closed after identity change")
	}
}

func TestNotifyWakeRestartLinuxRefusesChangedPendingRecord(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*wakeRestartRecord)
	}{
		{
			name: "request_id",
			mutate: func(record *wakeRestartRecord) {
				record.RequestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "generation",
			mutate: func(record *wakeRestartRecord) {
				record.Generation = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
		},
		{
			name: "not_pending",
			mutate: func(record *wakeRestartRecord) {
				record.Status = wakeRestartRefused
				record.Reason = "changed"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			signaled := false
			closed := false
			stubWakeRestartPidfdForTest(
				t,
				func(int, int) (int, error) {
					changed := fixture.record
					test.mutate(&changed)
					raw, err := json.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(
						filepath.Join(fixture.agentDir.path, wakeRestartFileName),
						append(raw, '\n'),
						0o600,
					); err != nil {
						t.Fatal(err)
					}
					return 44, nil
				},
				func(int, unix.Signal, *unix.Siginfo, int) error {
					signaled = true
					return nil
				},
				func(int) error {
					closed = true
					return nil
				},
			)

			err := notifyWakeRestartPlatform(fixture.agentDir, fixture.lock, fixture.record)
			if err == nil || !strings.Contains(err.Error(), "request changed before signal") {
				t.Fatalf("error = %v, want request-change refusal", err)
			}
			if signaled {
				t.Fatal("changed restart request was signaled")
			}
			if !closed {
				t.Fatal("pidfd was not closed after request change")
			}
		})
	}
}
