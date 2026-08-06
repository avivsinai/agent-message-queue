//go:build linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "pidfd_open") {
		t.Fatalf("retire result = %#v err=%v, want unsupported-pidfd failure", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was not preserved: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("target was not preserved: %v", err)
	}
}

func TestAcquireWakeLockLinuxDetachedValidReplacementUsesRetainedBoundState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	lock := fixture.created.Lock
	lock.PID = 4242
	lock.ProcessStart = "wake-start"
	lock.BootID = "boot-1"
	lock.Executable = "/usr/local/bin/amq"
	lock.Args = []string{"/usr/local/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", fixture.target.InjectVia}
	lock.TTY = "/dev/amq-missing-detached-test-tty"
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
	detachedPath := fixture.agentDir.path + ".valid-replacement-detached"
	var successorBefore map[string]detachedWakeFileSnapshot
	originalAfterRead := afterWakeLockAtRead
	afterWakeLockAtRead = func() {
		afterWakeLockAtRead = func() {}
		if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
			t.Fatalf("detach replacement wake agent directory: %v", err)
		}
		if err := os.Mkdir(fixture.agentDir.path, 0o700); err != nil {
			t.Fatalf("create replacement wake successor directory: %v", err)
		}
		copyDetachedWakeSuccessorFiles(t, detachedPath, fixture.agentDir.path)
		statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
		raw, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read successor wake state: %v", err)
		}
		state, err := decodeWakeState(raw)
		if err != nil {
			t.Fatalf("decode successor wake state: %v", err)
		}
		state.Target.TargetDigest = "sha256:" + strings.Repeat("0", 64)
		divergent, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal divergent successor wake state: %v", err)
		}
		if err := os.WriteFile(statePath, divergent, 0o600); err != nil {
			t.Fatalf("write divergent successor wake state: %v", err)
		}
		successorBefore = snapshotDetachedWakeFiles(
			t, fixture.agentDir.path, ".wake.lock", wakeTargetFileName, wakeStateFileName, wakePreparedFileName,
		)
	}
	t.Cleanup(func() { afterWakeLockAtRead = originalAfterRead })

	cleanup, err := acquireWakeLockWithOptionsInDir(
		fixture.agentDir, fixture.root, fixture.me, fixture.options,
	)
	if cleanup != nil {
		t.Fatal("detached replacement acquisition returned cleanup authority")
	}
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("detached replacement acquisition error = %v, want cleanup-only", err)
	}
	if !stopped {
		t.Fatal("replacement acquisition did not stop retained valid wake")
	}
	if successorBefore == nil {
		t.Fatal("replacement acquisition did not reach detached-directory seam")
	}
	assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}
