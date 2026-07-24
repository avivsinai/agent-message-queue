//go:build linux

package cli

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRetireWakeUsesLinuxPidfdAndPreservesTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stopped := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	opened := false
	stubLinuxPidfd(t,
		func(pid, flags int) (int, error) {
			if pid != wakePID || flags != 0 {
				t.Fatalf("pidfd_open(%d,%d)", pid, flags)
			}
			opened = true
			return 99, nil
		},
		func(fd int, sig unix.Signal, _ *unix.Siginfo, flags int) error {
			if !opened || fd != 99 || sig != unix.SIGTERM || flags != 0 {
				t.Fatalf("pidfd_send_signal(fd=%d,sig=%v,flags=%d), opened=%v", fd, sig, flags, opened)
			}
			stopped = true
			return nil
		},
		func(fd int, _ time.Duration) (bool, error) {
			if fd != 99 || !stopped {
				t.Fatalf("pidfd poll before capability stop: fd=%d stopped=%v", fd, stopped)
			}
			return true, nil
		},
	)

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("wake lock still exists: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("saved target was not preserved: %v", err)
	}
}
