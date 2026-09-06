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
	dir, err := os.MkdirTemp(cliSecureTempRoot, "amq-test-")
	if err != nil {
		t.Fatalf("create secure test temp directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove secure test temp directory: %v", err)
		}
	})
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
	lock.TargetDigest = mustWakeTargetDigest(target)
	return lock
}

func mustWakeTargetDigest(target wakeTarget) string {
	digest, err := wakeTargetDigest(target)
	if err != nil {
		panic(err)
	}
	return digest
}

func TestSameWakeInjectorIdentityUsesPathOrderedArgsAndRetryPolicy(t *testing.T) {
	first := wakeTarget{
		InjectVia:  "/opt/amq/injector",
		InjectArgs: []string{"exec", "target"},
		Created:    "2026-01-01T00:00:00Z",
		Owner:      &wakeOwner{PID: 101, ProcessStart: "first"},
	}
	second := first
	second.Created = "2026-07-20T00:00:00Z"
	second.Owner = &wakeOwner{PID: 202, ProcessStart: "second"}
	if !sameWakeInjectorIdentity(first, second) {
		t.Fatal("Created/owner metadata changed semantic injector identity")
	}

	second.InjectArgs = []string{"target", "exec"}
	if sameWakeInjectorIdentity(first, second) {
		t.Fatal("ordered fixed arguments were treated as interchangeable")
	}
	second = first
	second.InjectVia = "/opt/amq/other-injector"
	if sameWakeInjectorIdentity(first, second) {
		t.Fatal("different injector paths were treated as the same identity")
	}
	second = first
	second.RetryUntil = wakeRetryUntilInjected
	if sameWakeInjectorIdentity(first, second) {
		t.Fatal("different retry acknowledgement policies were treated as the same identity")
	}
	second = first
	second.RetryUntil = wakeRetryUntilDrained
	if !sameWakeInjectorIdentity(first, second) {
		t.Fatal("omitted and explicit drained policies were treated as different identities")
	}
}

func TestInspectWakeLockAcceptsLegacyDarwinBootIDForProvenWake(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "1783327533.465308000",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "start-1",
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "1783327533.407566000",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		t.Fatalf("inspection = status %q reason %q confirmed %v", inspection.Status, inspection.Reason, inspection.IdentityConfirmed)
	}
}
