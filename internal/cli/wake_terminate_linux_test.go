//go:build linux

package cli

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func stubLinuxPidfd(t *testing.T, open func(int, int) (int, error), send func(int, unix.Signal, *unix.Siginfo, int) error, poll func(int, time.Duration) (bool, error)) {
	t.Helper()
	oldOpen := linuxPidfdOpen
	oldSend := linuxPidfdSend
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = open
	linuxPidfdSend = send
	linuxPidfdPoll = poll
	linuxPidfdClose = func(int) error { return nil }
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdSend = oldSend
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
	})
}

func matchingLinuxWakeProcess(pid int, root string) wakeProcessInfo {
	return wakeProcessInfo{
		PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
		Args: []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
	}
}

func TestTerminateWakePidfdRefusesImmortalWithinKillConfirm(t *testing.T) {
	var timeouts []time.Duration
	killPollReportedAlive := false
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 7, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { return nil },
		func(_ int, timeout time.Duration) (bool, error) {
			timeouts = append(timeouts, timeout)
			if timeout == wakeTerminateKillConfirm {
				killPollReportedAlive = true
			}
			return false, nil
		},
	)
	err := terminateWakePidfd(7)
	if err == nil || !strings.Contains(err.Error(), "still alive after SIGKILL") {
		t.Fatalf("immortal process error = %v, want SIGKILL confirmation refusal", err)
	}
	if !killPollReportedAlive {
		t.Fatal("refused before the SIGKILL poll reported not-exited")
	}
	if len(timeouts) != 2 || timeouts[0] != wakeTerminateGrace || timeouts[1] != wakeTerminateKillConfirm {
		t.Fatalf("pidfd poll timeouts = %v, want [%s %s]", timeouts, wakeTerminateGrace, wakeTerminateKillConfirm)
	}
}
