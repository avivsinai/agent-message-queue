//go:build darwin || linux

package cli

import (
	"encoding/json"
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

func TestRetireWakeRefusesLiveRawWake(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/ttys001",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "0123456789abcdef0123456789abcdef",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	requested := mustNewWakeTargetForTest(t, root, "codex", injector, nil)

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "raw wake") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("raw wake lock changed: %v", err)
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
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "different injector") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("mismatched wake lock changed: %v", err)
	}
}

func TestRetireWakeRemovesExactlyBoundProvenStaleLock(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired" || !strings.Contains(result.Reason, "proven-stale") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock still exists: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("saved target was not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), "inbox")); err != nil {
		t.Fatalf("mailbox was not preserved: %v", err)
	}
}

func TestRetireWakeRefusesMissingSavedTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-a"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID: wakePID, ProcessStart: "wake-start", BootID: "boot-1", Generation: "0123456789abcdef0123456789abcdef",
	}, requested))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "no saved inject-via wake target") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock changed after missing-target refusal: %v", err)
	}
}

func TestRunWakeRetireRequiresExpectedInjectVia(t *testing.T) {
	err := runWake([]string{"retire", "--root", secureTempDirForTest(t), "--me", "codex"})
	if err == nil || !strings.Contains(err.Error(), "--inject-via is required") {
		t.Fatalf("runWake retire error = %v", err)
	}
}

func TestRunWakeRetireJSONReportsRefusal(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWake([]string{"retire", "--root", root, "--me", "codex", "--inject-via", injector, "--json"})
	})
	if runErr == nil {
		t.Fatal("missing wake lock unexpectedly retired")
	}
	var result wakeRetireResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nstdout=%s", err, stdout)
	}
	if result.Status != "refused" || result.Agent != "codex" || result.Root != canonicalWakeRoot(root) {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunWakeRetireTextReportsRefusal(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWake([]string{"retire", "--root", root, "--me", "codex", "--inject-via", injector})
	})
	if runErr == nil {
		t.Fatal("missing wake lock unexpectedly retired")
	}
	if !strings.Contains(stdout, "wake retire: refused agent=codex") ||
		!strings.Contains(stdout, "reason=no wake lock present") {
		t.Fatalf("stdout=%q", stdout)
	}
}
