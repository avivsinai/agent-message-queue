//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func stubInspectWakeProcess(t *testing.T, fn func(pid int) wakeProcessInfo) {
	t.Helper()
	old := inspectWakeProcess
	inspectWakeProcess = fn
	t.Cleanup(func() {
		inspectWakeProcess = old
	})
}

func writeWakeLockForTest(t *testing.T, root, agent string, lock wakeLock) string {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if lock.Root == "" {
		lock.Root = canonicalWakeRoot(root)
	}
	if lock.Agent == "" {
		lock.Agent = agent
	}
	if lock.Started == "" {
		lock.Started = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal wake lock: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, agent), ".wake.lock")
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write wake lock: %v", err)
	}
	return lockPath
}

func TestRunWakeWithLoopInjectViaSkipsTTYStartupRequirement(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	var got wakeConfig
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", "/tmp/inject tool",
		"--inject-arg", "exec",
		"--inject-arg", "Team Alpha",
		"--inject-timeout", "250ms",
	}, func(cfg wakeConfig) error {
		got = cfg
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
	if got.injectVia != "/tmp/inject tool" {
		t.Fatalf("expected inject executable with spaces, got %q", got.injectVia)
	}
	if strings.Join(got.injectArgs, "|") != "exec|Team Alpha" {
		t.Fatalf("expected fixed inject args, got %#v", got.injectArgs)
	}
	if got.injectTimeout != 250*time.Millisecond {
		t.Fatalf("expected inject timeout 250ms, got %s", got.injectTimeout)
	}
}

func TestRunWakeWithLoopWritesReadyFileAfterLock(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", "/tmp/injector",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file before wake loop: %v", statErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopDoesNotWriteReadyFileWhenLockBlocked(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := t.TempDir()
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", "/tmp/injector",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected existing wake lock error")
	}
	if !strings.Contains(err.Error(), "wake already running") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestAcquireWakeLockSelfHealsPIDReusedByNonAMQ(t *testing.T) {
	const reusedPID = 4242
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		if pid == os.Getpid() {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "self-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator")
	if err != nil {
		t.Fatalf("acquireWakeLock should replace stale PID-reuse lock: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.ProcessStart != "self-start" {
		t.Fatalf("replacement process_start = %q, want self-start", got.ProcessStart)
	}
}

func TestAcquireWakeLockSelfHealsPIDReusedByDifferentAMQStart(t *testing.T) {
	const reusedPID = 4242
	root := t.TempDir()
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		if pid == os.Getpid() {
			return wakeProcessInfo{PID: pid, Running: true, StartToken: "self-start", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator")
	if err != nil {
		t.Fatalf("acquireWakeLock should replace mismatched start-token lock: %v", err)
	}
	defer cleanup()
}

func TestAcquireWakeLockSelfHealsStartMismatchWhenExecutableUnavailable(t *testing.T) {
	const reusedPID = 4242
	root := t.TempDir()
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "new-start",
				BootID:       "boot-1",
				InspectError: errors.New("executable unavailable"),
			}
		}
		if pid == os.Getpid() {
			return wakeProcessInfo{PID: pid, Running: true, StartToken: "self-start", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator")
	if err != nil {
		t.Fatalf("acquireWakeLock should replace start-token mismatch even without executable: %v", err)
	}
	defer cleanup()
}

func TestAcquireWakeLockStartReadFailureIsUnverifiedNotMismatch(t *testing.T) {
	const pid = 4242
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          pid,
		ProcessStart: "old-start",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:          gotPID,
				Running:      true,
				Executable:   "/opt/homebrew/bin/amq",
				InspectError: errors.New("permission denied"),
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator")
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected unverified lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected unverified error, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("unverified lock should remain, stat=%v", statErr)
	}
}

func TestAcquireWakeLockLegacyLiveLockDoesNotAutoDelete(t *testing.T) {
	const pid = 4242
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:        pid,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator")
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected legacy unverified lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected unverified error, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("legacy unverified lock should remain, stat=%v", statErr)
	}
}

func TestRemoveWakeLockIfUnchangedRefusesChangedLock(t *testing.T) {
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{PID: 4242})
	inspection := inspectWakeLock(root, "orchestrator")
	if !inspection.Exists {
		t.Fatal("expected lock inspection")
	}
	changed := wakeLock{
		PID:     4243,
		Root:    canonicalWakeRoot(root),
		Agent:   "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(changed)
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write changed lock: %v", err)
	}

	err := removeWakeLockIfUnchanged(inspection)
	if err == nil {
		t.Fatal("expected changed lock removal error")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("changed lock should remain, stat=%v", statErr)
	}
}

func TestShouldReplaceOrphanedWakeLockReverifiesBeforeSignal(t *testing.T) {
	const pid = 4242
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          pid,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	inspections := 0
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			inspections++
			if inspections == 1 {
				return wakeProcessInfo{
					PID:        gotPID,
					Running:    true,
					StartToken: "start-1",
					BootID:     "boot-1",
					Executable: "/opt/homebrew/bin/amq",
					Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				}
			}
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				StartToken: "start-2",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	inspection := inspectWakeLock(root, "orchestrator")
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		t.Fatalf("expected initial valid confirmed lock, got status=%s confirmed=%v", inspection.Status, inspection.IdentityConfirmed)
	}
	if shouldReplaceOrphanedWakeLock(inspection) {
		t.Fatal("should not replace when recheck no longer confirms the same wake lock")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should remain after failed recheck: %v", err)
	}
	if inspections < 2 {
		t.Fatalf("expected re-inspection before replacement, got %d inspection(s)", inspections)
	}
}

func TestRunWakeWithLoopRejectsRecentlyCorruptLock(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt lock: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", "/tmp/injector",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with recent corrupt lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected recent corrupt lock error")
	}
	if !strings.Contains(err.Error(), "being created") {
		t.Fatalf("expected being-created error, got %v", err)
	}
}

func TestWaitForWakeReadyReturnsWhenReadyFileAppears(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", `sleep 0.05; : > "$1"; sleep 1`, "sh", readyPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	if err := waitForWakeReady(cmd.Process, readyPath, time.Second); err != nil {
		t.Fatalf("waitForWakeReady: %v", err)
	}
}

func TestWaitForWakeReadyFailsWhenWakeExitsBeforeReady(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	err := waitForWakeReady(cmd.Process, readyPath, time.Second)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "amq wake exited before becoming ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunWakeWithLoopRejectsInjectArgWithoutInjectVia(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid flags: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-arg requires --inject-via") {
		t.Fatalf("expected inject-arg usage error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsNonPositiveInjectTimeout(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-via", "/tmp/injector",
		"--inject-timeout", "0",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid timeout: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-timeout must be > 0") {
		t.Fatalf("expected inject-timeout usage error, got %v", err)
	}
}

func TestWakeHealthCheckSkipsTTYForInjectVia(t *testing.T) {
	err := wakeHealthCheck(wakeConfig{injectVia: "/tmp/injector"}, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected external injection health check to skip TTY, got %v", err)
	}
}

func TestWakeHealthCheckRequiresTTYForTIOCSTI(t *testing.T) {
	err := wakeHealthCheck(wakeConfig{}, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected TTY health failure")
	}
	if err.Error() != "TTY no longer available" {
		t.Fatalf("expected TTY health error, got %v", err)
	}
}
