//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func stubSignalWakeProcess(t *testing.T, fn func(pid int, sig os.Signal) error) {
	t.Helper()
	oldSignal := signalWakeProcess
	oldGrace := wakeTerminateGrace
	signalWakeProcess = fn
	wakeTerminateGrace = 0
	t.Cleanup(func() {
		signalWakeProcess = oldSignal
		wakeTerminateGrace = oldGrace
	})
}

func stubWakeCurrentTTY(t *testing.T, fn func() string) {
	t.Helper()
	old := getWakeCurrentTTY
	getWakeCurrentTTY = fn
	t.Cleanup(func() {
		getWakeCurrentTTY = old
	})
}

func stubWakeProcessSID(t *testing.T, fn func(pid int) (int, error)) {
	t.Helper()
	old := getWakeProcessSID
	getWakeProcessSID = fn
	t.Cleanup(func() {
		getWakeProcessSID = old
	})
}

func writeExecutableForTest(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(secureTempDirForTest(t), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func TestRunWakeWithLoopInjectViaSkipsTTYStartupRequirement(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	var got wakeConfig
	errDone := errors.New("done")
	injector := writeExecutableForTest(t, "inject tool")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
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
	if got.injectVia != injector {
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
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
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

func TestRunWakeWithLoopWritesInjectViaWakeTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		target, exists, targetErr := readWakeTarget(root, "orchestrator")
		if targetErr != nil {
			t.Fatalf("readWakeTarget: %v", targetErr)
		}
		if !exists {
			t.Fatal("expected wake target to be written")
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopExecutesResolvedInjectViaPath(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	base := secureTempDirForTest(t)
	realDir := filepath.Join(base, "real-bin")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real bin: %v", err)
	}
	injector := filepath.Join(realDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	linkDir := filepath.Join(base, "link-bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink bin: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", filepath.Join(linkDir, "injector"),
	}, func(cfg wakeConfig) error {
		if cfg.injectVia != injector {
			t.Fatalf("cfg.injectVia = %q, want resolved %q", cfg.injectVia, injector)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsUnsafeInjectViaBeforeLoop(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--inject-arg", "exec",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	if _, exists, targetErr := readWakeTarget(root, "orchestrator"); targetErr != nil || exists {
		t.Fatalf("wake target exists=%v err=%v, want absent with no read error", exists, targetErr)
	}
}

func TestInjectViaRevalidatesExecutableBeforeExec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, path string)
		wantText string
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := writeExecutableForTest(t, "target-injector")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink injector: %v", err)
				}
			},
			wantText: "must not be a symlink",
		},
		{
			name: "nonregular",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir injector path: %v", err)
				}
			},
			wantText: "must be a regular file",
		},
		{
			name: "world_writable",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o777); err != nil {
					t.Fatalf("chmod injector: %v", err)
				}
			},
			wantText: "group/world-writable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injector := writeExecutableForTest(t, "injector-"+tc.name)
			cfg := &wakeConfig{
				injectVia:     injector,
				injectTimeout: time.Second,
			}
			tc.mutate(t, injector)

			err := injectVia(cfg, "payload")
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q rejection, got %v", tc.wantText, err)
			}
		})
	}
}

func TestRunWakeWithLoopKeepsOldWakeTargetWhenNewTargetIsUnsafe(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	oldInjector := writeExecutableForTest(t, "old-injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", oldInjector, []string{"old"})); err != nil {
		t.Fatalf("write old wake target: %v", err)
	}
	newInjector := writeExecutableForTest(t, "new-injector")
	if err := os.Chmod(newInjector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", newInjector,
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	target, exists, err := readWakeTarget(root, "orchestrator")
	if err != nil || !exists {
		t.Fatalf("wake target exists=%v err=%v, want old target retained", exists, err)
	}
	if target.InjectVia != oldInjector {
		t.Fatalf("wake target inject_via = %q, want old target %q", target.InjectVia, oldInjector)
	}
}

func TestRunWakeWithLoopRejectsInjectorSwappedAfterTargetWrite(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	if err := os.Remove(injector); err != nil {
		t.Fatalf("remove injector: %v", err)
	}
	if err := os.Symlink("/bin/sh", injector); err != nil {
		t.Fatalf("swap injector to symlink: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run after injector swap: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected swapped injector rejection, got %v", err)
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
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
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

func TestRunWakeWithLoopWritesReadyFileForExistingUsableWake(t *testing.T) {
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
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err != nil {
		t.Fatalf("expected existing usable wake to satisfy ready file, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsMissingTTY(t *testing.T) {
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
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for --require-wake") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsBlankOrUnknownTTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  string
	}{
		{name: "blank", tty: ""},
		{name: "unknown", tty: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			root := secureTempDirForTest(t)
			lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
				PID:          wakePID,
				TTY:          tc.tty,
				ProcessStart: "start-1",
				BootID:       "boot-1",
				Executable:   "/opt/homebrew/bin/amq",
			})

			readyPath := filepath.Join(t.TempDir(), "wake.ready")
			injector := writeExecutableForTest(t, "injector")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", "orchestrator",
				"--inject-via", injector,
				"--ready-file", readyPath,
				"--accept-existing-wake",
			}, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
				return nil
			})
			if err == nil {
				t.Fatal("expected unusable wake lock error")
			}
			if !strings.Contains(err.Error(), "not usable for --require-wake") {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ready file should not exist, statErr=%v", statErr)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("existing lock should remain, statErr=%v", statErr)
			}
		})
	}
}

func TestRunWakeWithLoopAcceptExistingWakeAcceptsInjectViaUnknownTTY(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err != nil {
		t.Fatalf("expected inject-via wake to satisfy ready file despite unknown tty, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsSameTTYDifferentSession(t *testing.T) {
	const wakePID = 4242
	ttyPath := filepath.Join(t.TempDir(), "amq-test-tty")
	if err := os.WriteFile(ttyPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write fake tty path: %v", err)
	}
	stubWakeCurrentTTY(t, func() string { return ttyPath })
	sidCalls := 0
	stubWakeProcessSID(t, func(pid int) (int, error) {
		sidCalls++
		if pid == wakePID {
			return 100, nil
		}
		return 200, nil
	})
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
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          ttyPath,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for --require-wake") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
	if sidCalls < 2 {
		t.Fatalf("expected same-TTY branch to inspect wake and current SIDs, got %d calls", sidCalls)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsUnverifiedWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "test-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unverified wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unverified wake lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestAcquireWakeLockSelfHealsPIDReusedByNonAMQ(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
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

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
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
	root := secureTempDirForTest(t)
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

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should replace mismatched start-token lock: %v", err)
	}
	defer cleanup()
}

func TestAcquireWakeLockSelfHealsStartMismatchWhenExecutableUnavailable(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
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

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should replace start-token mismatch even without executable: %v", err)
	}
	defer cleanup()
}

func TestAcquireWakeLockStartReadFailureIsUnverifiedNotMismatch(t *testing.T) {
	const pid = 4242
	root := secureTempDirForTest(t)
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

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
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
	root := secureTempDirForTest(t)
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

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
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
	root := secureTempDirForTest(t)
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

func TestInspectWakeLockRejectsSymlinkAndFIFO(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, path string)
		wantError string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "lock.json")
				if err := os.WriteFile(target, []byte(`{"pid":4242}`), 0o600); err != nil {
					t.Fatalf("write target lock: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink lock: %v", err)
				}
			},
			wantError: "must not be a symlink",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo lock: %v", err)
				}
			},
			wantError: "must be a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentBase := fsq.AgentBase(root, "orchestrator")
			if err := os.MkdirAll(agentBase, 0o700); err != nil {
				t.Fatalf("mkdir agent base: %v", err)
			}
			tc.setup(t, filepath.Join(agentBase, ".wake.lock"))

			done := make(chan wakeLockInspection, 1)
			go func() {
				done <- inspectWakeLock(root, "orchestrator")
			}()

			select {
			case inspection := <-done:
				if !inspection.Exists || inspection.Status != wakeLockUnverified ||
					!strings.Contains(inspection.Reason, tc.wantError) {
					t.Fatalf("unexpected inspection: %#v", inspection)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("inspectWakeLock blocked")
			}
		})
	}
}

func TestShouldReplaceOrphanedWakeLockSignalsOnlyAfterRevalidation(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			if killed {
				return wakeProcessInfo{PID: pid, Running: false}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	signals := []os.Signal{}
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		signals = append(signals, sig)
		if sig == os.Kill {
			killed = true
		}
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err != nil {
		t.Fatalf("shouldReplaceOrphanedWakeLock: %v", err)
	}
	if !replaced {
		t.Fatal("expected confirmed orphan to be replaced")
	}
	if len(signals) != 2 {
		t.Fatalf("signals = %d, want SIGTERM and SIGKILL", len(signals))
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock should be removed after confirmed orphan replacement, stat=%v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockKeepsLockWhenKillDoesNotTerminate(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "still alive after SIGKILL") {
		t.Fatalf("expected still-alive error, got %v", err)
	}
	if replaced {
		t.Fatal("should not replace lock when old wake remains alive")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after failed kill, stat=%v", statErr)
	}
}

func TestShouldReplaceOrphanedWakeLockRevalidatesBeforeSignal(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			inspectCalls++
			if inspectCalls <= 2 {
				return wakeProcessInfo{
					PID:        pid,
					Running:    true,
					StartToken: "start-1",
					BootID:     "boot-1",
					Executable: "/opt/homebrew/bin/amq",
					Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "reused-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("must not signal after process identity changes, got pid=%d sig=%v", pid, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil {
		t.Fatal("expected identity-changed error")
	}
	if replaced {
		t.Fatal("should not replace lock when process identity changes before signal")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after aborted signal, stat=%v", statErr)
	}
}

func TestRunWakeWithLoopRejectsRecentlyCorruptLock(t *testing.T) {
	root := secureTempDirForTest(t)
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
		"--inject-via", writeExecutableForTest(t, "injector"),
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

func TestWaitForWakeReadyAcceptsReadyFileWrittenBeforeExit(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", `: > "$1"; exit 0`, "sh", readyPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	if err := waitForWakeReady(cmd.Process, readyPath, time.Second); err != nil {
		t.Fatalf("waitForWakeReady should accept ready file written before exit: %v", err)
	}
}

func TestConfigureRepairWakeCommandDetachesOutput(t *testing.T) {
	output, err := os.OpenFile(filepath.Join(t.TempDir(), "repair.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer func() { _ = output.Close() }()

	cmd := exec.Command("amq")
	configureRepairWakeCommand(cmd, output)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("repair wake command should start in a new session: %#v", cmd.SysProcAttr)
	}
	if cmd.Stdout != output || cmd.Stderr != output {
		t.Fatalf("repair wake command should redirect stdout/stderr to repair log")
	}
	if cmd.Stdout == os.Stdout || cmd.Stderr == os.Stderr {
		t.Fatalf("repair wake command must not inherit parent stdout/stderr")
	}
}

func TestOpenWakeRepairOutputCreatesPrivateLog(t *testing.T) {
	root := secureTempDirForTest(t)
	output, err := openWakeRepairOutput(root, "orchestrator")
	if err != nil {
		t.Fatalf("openWakeRepairOutput: %v", err)
	}
	path := output.Name()
	if err := output.Close(); err != nil {
		t.Fatalf("close output: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat repair output: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("repair output mode = %o, want 0600", got)
	}
}

func TestOpenWakeRepairOutputRejectsSymlinkLog(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	target := filepath.Join(t.TempDir(), "repair.log")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(agentBase, ".wake.repair.log")); err != nil {
		t.Fatalf("symlink repair log: %v", err)
	}

	output, err := openWakeRepairOutput(root, "orchestrator")
	if err == nil {
		_ = output.Close()
		t.Fatal("expected symlink repair log rejection")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestOpenWakeRepairOutputRejectsFIFOWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		output, err := openWakeRepairOutput(root, "orchestrator")
		if output != nil {
			_ = output.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
			t.Fatalf("expected FIFO rejection, got %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("openWakeRepairOutput blocked on FIFO")
	}
}

func TestRunWakeRepairJSONRejectsFIFOLogWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "orchestrator", "--json"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "regular file") {
		t.Fatalf("runWakeRepair error = %v, want regular-file refusal", runErr)
	}
	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "error" || !strings.Contains(result.Reason, "regular file") {
		t.Fatalf("unexpected result: %#v", result)
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
		"--inject-via", writeExecutableForTest(t, "injector"),
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
