//go:build linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func TestRetireWakeUsesLinuxPidfdAndRemovesTarget(t *testing.T) {
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
	if _, err := os.Stat(wakeTargetPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("retired target still exists: %v", err)
	}
}

func TestRetireWakeLinuxUsesRetainedDirectoryAfterAgentReplacement(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, _ := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stopped := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 99, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { stopped = true; return nil },
		func(int, time.Duration) (bool, error) { return stopped, nil },
	)
	detachedPath, successorLock := installRetireAgentDirectoryReplacement(
		t, root, "codex", ".wake.lock", wakeTargetFileName,
	)

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, ".wake.lock")); !os.IsNotExist(err) {
		t.Fatalf("retained exact lock survived retirement: %v", err)
	}
	assertRetireReplacementLockPreserved(t, root, "codex", successorLock())
}

func TestRetireWakeLinuxPreservesLiveBoundStateReplacement(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := inspectWakeLock(fixture.root, fixture.me).Lock
	lock.Executable = "/usr/local/bin/amq"
	lock.Args = []string{"/usr/local/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", requested.InjectVia}
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	stopped := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return matchingRetireWakeProcessFromLock(pid, lock)
	})
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 99, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { stopped = true; return nil },
		func(int, time.Duration) (bool, error) { return stopped, nil },
	)
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	stateRaw, installReplacement := stageExactWakeArtifactReplacement(t, statePath, ".live-replacement")
	originalHook := afterWakeRetireLockRemoval
	afterWakeRetireLockRemoval = func() {
		if err := installReplacement(); err != nil {
			t.Errorf("install live bound state replacement: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireLockRemoval = originalHook })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target removed with live replacement state: %v", err)
	}
	assertFileRawForTest(t, statePath, stateRaw)
}

func TestRetireWakeLinuxDetachedBoundStateValidationUsesRetainedDirectory(t *testing.T) {
	tests := []struct {
		name           string
		successorFiles []string
		divergeState   bool
	}{
		{
			name:           "missing successor state",
			successorFiles: []string{".wake.lock", wakeTargetFileName},
		},
		{
			name:           "divergent successor state",
			successorFiles: []string{".wake.lock", wakeTargetFileName, wakeStateFileName},
			divergeState:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, requested, stopped := setupBoundLinuxRetire(t)
			detachedPath, successorLock := installRetireAgentDirectoryReplacement(
				t, fixture.root, fixture.me, test.successorFiles...,
			)
			if test.divergeState {
				originalPoll := linuxPidfdPoll
				linuxPidfdPoll = func(fd int, timeout time.Duration) (bool, error) {
					exited, err := originalPoll(fd, timeout)
					if !exited || err != nil {
						return exited, err
					}
					statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
					raw, readErr := os.ReadFile(statePath)
					if readErr != nil {
						t.Fatalf("read successor state: %v", readErr)
					}
					state, decodeErr := decodeWakeState(raw)
					if decodeErr != nil {
						t.Fatalf("decode successor state: %v", decodeErr)
					}
					state.Target.TargetDigest = "sha256:" + strings.Repeat("0", 64)
					divergent, marshalErr := json.Marshal(state)
					if marshalErr != nil {
						t.Fatalf("marshal divergent successor state: %v", marshalErr)
					}
					if writeErr := os.WriteFile(statePath, divergent, 0o600); writeErr != nil {
						t.Fatalf("write divergent successor state: %v", writeErr)
					}
					return true, nil
				}
				t.Cleanup(func() { linuxPidfdPoll = originalPoll })
			}

			result, err := retireWake(fixture.root, fixture.me, requested)
			if !*stopped {
				t.Fatal("pidfd retirement did not stop the retained wake")
			}
			if err != nil || result.Status != "retired_with_residue" {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if strings.Contains(result.Reason, "wake lock durability") ||
				!strings.Contains(result.Reason, "detached wake cleanup") {
				t.Fatalf("detached residue reason = %q", result.Reason)
			}
			assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
			assertRetireReplacementLockPreserved(t, fixture.root, fixture.me, successorLock())
		})
	}
}

func TestRetireWakeLinuxRemovesRetainedLockWhenCanonicalDirectoryDisappearsAfterStop(t *testing.T) {
	fixture, requested, stopped := setupBoundLinuxRetire(t)
	detachedPath := fixture.agentDir.path + ".post-stop-no-successor"
	originalPoll := linuxPidfdPoll
	linuxPidfdPoll = func(int, time.Duration) (bool, error) {
		if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
			t.Fatalf("detach agent directory after pidfd stop: %v", err)
		}
		return true, nil
	}
	t.Cleanup(func() { linuxPidfdPoll = originalPoll })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if !*stopped {
		t.Fatal("pidfd retirement did not stop wake before namespace loss")
	}
	if err != nil || result.Status != "retired_with_residue" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if strings.Contains(result.Reason, "wake lock durability") ||
		!strings.Contains(result.Reason, "detached wake cleanup") {
		t.Fatalf("detached residue reason = %q", result.Reason)
	}
	assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
}

func setupBoundLinuxRetire(
	t *testing.T,
) (*genericWakePreparedCleanupFixture, wakeTarget, *bool) {
	t.Helper()
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := inspectWakeLock(fixture.root, fixture.me).Lock
	lock.Executable = "/usr/local/bin/amq"
	lock.Args = []string{"/usr/local/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", requested.InjectVia}
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	stopped := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if stopped {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return matchingRetireWakeProcessFromLock(pid, lock)
	})
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 99, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { stopped = true; return nil },
		func(int, time.Duration) (bool, error) { return stopped, nil },
	)
	return fixture, requested, &stopped
}
