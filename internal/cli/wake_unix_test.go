//go:build darwin || linux

package cli

import (
	"errors"
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
