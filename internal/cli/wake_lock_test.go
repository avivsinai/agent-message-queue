package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func secureTempDirForTest(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
			cwd = resolved
		}
		dir, mkErr := os.MkdirTemp(cwd, ".amq-secure-test-")
		if mkErr == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	dir := t.TempDir()
	if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
		return resolved
	}
	return dir
}

func mustNewWakeTargetForTest(t *testing.T, root, me, injectVia string, injectArgs []string) wakeTarget {
	t.Helper()
	target, err := newWakeTarget(root, me, injectVia, injectArgs)
	if err != nil {
		t.Fatalf("newWakeTarget: %v", err)
	}
	return target
}

func writeWakeLockForTest(t *testing.T, root, agent string, lock wakeLock) string {
	t.Helper()
	if lock.Root == "" {
		lock.Root = canonicalWakeRoot(root)
	}
	if lock.Agent == "" {
		lock.Agent = agent
	}
	return writeWakeLockExactForTest(t, root, agent, lock)
}

func writeWakeLockExactForTest(t *testing.T, root, agent string, lock wakeLock) string {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
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

func bindWakeLockToTarget(lock wakeLock, target wakeTarget) wakeLock {
	lock.WakeMode = wakeTargetInjectVia
	lock.TargetDigest = wakeTargetDigest(target)
	return lock
}
