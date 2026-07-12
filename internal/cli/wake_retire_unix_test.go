//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func installRetireWakeFixture(t *testing.T, root, me, injector string, args []string, pid int) (wakeTarget, string) {
	t.Helper()
	target := mustNewWakeTargetForTest(t, root, me, injector, args)
	lockPath := writeWakeLockForTest(t, root, me, bindWakeLockToTarget(wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, me, target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	return target, lockPath
}

func matchingRetireWakeProcess(pid int, root, me, injector string) wakeProcessInfo {
	return wakeProcessInfo{
		PID:        pid,
		Running:    true,
		StartToken: "wake-start",
		BootID:     "boot-1",
		Executable: "/opt/homebrew/bin/amq",
		Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", me, "--root", root, "--inject-via", injector},
	}
}

func TestRetireWakeStopsExactIdentityAndPreservesMailboxTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "amq-keepalive")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector,
		[]string{"inject", "cmux", "cmux:surface:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}, wakePID)

	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		if killed {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		killed = true
		return nil
	})

	result, err := retireWake(root, "codex", requested)
	if err != nil {
		t.Fatalf("retireWake: %v", err)
	}
	if result.Status != "retired" || result.PID != wakePID {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("wake lock should be removed, stat=%v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("saved target should be preserved, stat=%v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), "inbox")); err != nil {
		t.Fatalf("mailbox should be preserved, stat=%v", err)
	}
}

func TestRetireWakeRefusesDifferentInjectTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "amq-keepalive")
	_, lockPath := installRetireWakeFixture(t, root, "codex", injector,
		[]string{"inject", "cmux", "cmux:surface:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}, wakePID)
	requested := mustNewWakeTargetForTest(t, root, "codex", injector,
		[]string{"inject", "cmux", "cmux:surface:11111111-2222-3333-4444-555555555555"})

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("mismatched target must not be signaled, got pid=%d sig=%v", pid, sig)
		return nil
	})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "does not match") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("mismatched wake lock should remain, stat=%v", err)
	}
}

func TestRetireWakeRevalidatesIdentityBeforeSignal(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "amq-keepalive")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector,
		[]string{"inject", "cmux", "cmux:surface:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}, wakePID)

	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		if inspectCalls <= 2 {
			return matchingRetireWakeProcess(pid, root, "codex", injector)
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "reused-start",
			BootID:     "boot-1",
			Executable: "/bin/sleep",
			Args:       []string{"/bin/sleep", "100"},
		}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("changed process identity must not be signaled, got pid=%d sig=%v", pid, sig)
		return nil
	})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "error" || !strings.Contains(result.Reason, "identity changed") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("wake lock should remain after identity change, stat=%v", err)
	}
}

func TestRetireWakeRemovesExactStaleLockWithoutSignal(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "amq-keepalive")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector,
		[]string{"inject", "cmux", "cmux:surface:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("stale wake must not be signaled, got pid=%d sig=%v", pid, sig)
		return nil
	})

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale wake lock should be removed, stat=%v", err)
	}
}

func TestRetireWakeRefusesWhenLockIsAbsent(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "amq-keepalive")
	requested := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"inject", "cmux", "surface"})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "cannot be proven") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
