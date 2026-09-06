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
	if err := writeWakeTarget(root, me, target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	lockPath := writeWakeLockForTest(t, root, me, bindWakeLockToTarget(wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", me, "--inject-via", injector},
		Generation:   "0123456789abcdef0123456789abcdef",
	}, target))
	return target, lockPath
}

func matchingRetireWakeProcess(pid int, root, me, injector string) wakeProcessInfo {
	return wakeProcessInfo{
		PID:        pid,
		Running:    true,
		StartToken: "wake-start",
		BootID:     "boot-1",
		Executable: "/opt/homebrew/bin/amq",
		Args:       []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", me, "--inject-via", injector},
	}
}

func TestRetireWakeRefusesDifferentInjectTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	_, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	requested := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-b"})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "different injector identity") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("mismatched wake lock changed: %v", err)
	}
}

func TestRetireWakeRemovesBoundTargetAndStateSoTargetlessWakeCanStart(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); !os.IsNotExist(err) {
		t.Fatalf("retired wake target still exists: %v", err)
	}
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("retired wake state still exists: %v", err)
	}

	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{
		wakeMode: wakeInjectModeNone,
	})
	if err != nil {
		t.Fatalf("start targetless wake after retirement: %v", err)
	}
	cleanup()
}
