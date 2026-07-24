//go:build linux

package cli

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWakeUpgradeRetireUnsupportedPidfdFailsClosed(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return -1, syscall.ENOSYS },
		func(int, unix.Signal, *unix.Siginfo, int) error {
			t.Fatal("retirement must not signal without a pidfd")
			return nil
		},
		func(int, time.Duration) (bool, error) {
			t.Fatal("retirement must not poll without a pidfd")
			return false, nil
		},
	)

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "error" || !strings.Contains(result.Reason, "pidfd_open") {
		t.Fatalf("retire result = %#v err=%v, want unsupported-pidfd failure", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was not preserved: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("target was not preserved: %v", err)
	}
}
