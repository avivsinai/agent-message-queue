//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	cleanup, err := acquireWakeLock(root, "orchestrator")
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err = runWakeWithLoop([]string{
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
