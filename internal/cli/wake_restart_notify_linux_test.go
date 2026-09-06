//go:build linux

package cli

import (
	"reflect"
	"strings"
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
