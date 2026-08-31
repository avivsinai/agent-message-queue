//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "different injector identity") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("mismatched wake lock changed: %v", err)
	}
}

func TestRetireWakeRefusesRetryPolicyMismatch(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	persisted, _ := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	persisted.RetryUntil = wakeRetryUntilInjected
	if err := writeWakeTarget(root, "codex", persisted); err != nil {
		t.Fatalf("rewrite wake target with retry policy: %v", err)
	}
	// Rebind lock digest to the injected-policy target bytes.
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   "0123456789abcdef0123456789abcdef",
	}, persisted))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	// Requested/default is drained (empty RetryUntil); persisted is injected.
	requested := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-a"})

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "retry acknowledgement policy") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("retry-policy mismatch changed lock: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("retry-policy mismatch changed target: %v", err)
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
	if _, err := os.Stat(wakeTargetPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("retired target still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), "inbox")); err != nil {
		t.Fatalf("mailbox was not preserved: %v", err)
	}
}

func TestRetireWakePreservesTargetReplacementAfterExactLockRemoval(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	replacement := requested
	replacement.InjectArgs = []string{"exec", "terminal-replacement"}
	replacementPath := wakeTargetPath(root, "codex") + ".replacement"
	replacementRaw, err := json.MarshalIndent(replacement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, append(replacementRaw, '\n'), 0o600); err != nil {
		t.Fatalf("write replacement target: %v", err)
	}
	originalHook := afterWakeRetireLockRemoval
	afterWakeRetireLockRemoval = func() {
		if err := os.Rename(replacementPath, wakeTargetPath(root, "codex")); err != nil {
			t.Errorf("install replacement target: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireLockRemoval = originalHook })

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, wakeTargetFileName) ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("exact stale lock survived retirement: %v", err)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeTarget(persisted, replacement) {
		t.Fatalf("replacement target was not preserved: target=%#v exists=%v err=%v", persisted, exists, err)
	}
}

func TestRetireWakePreservesInodeOnlyTargetReplacementAfterCommit(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	targetPath := wakeTargetPath(root, "codex")
	targetRaw, installReplacement := stageExactWakeArtifactReplacement(t, targetPath, ".same-bytes-replacement")
	targetBefore, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	originalHook := afterWakeRetireLockRemoval
	afterWakeRetireLockRemoval = func() {
		if err := installReplacement(); err != nil {
			t.Errorf("install inode-only target replacement: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireLockRemoval = originalHook })

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, wakeTargetFileName) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("exact stale lock survived retirement: %v", err)
	}
	targetAfter, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(targetBefore, targetAfter) {
		t.Fatal("target replacement did not change inode")
	}
	assertFileRawForTest(t, targetPath, targetRaw)
}

func TestRetireWakePreservesStateReplacementAfterExactLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	stateRaw, installReplacement := stageExactWakeArtifactReplacement(t, statePath, ".replacement")
	stateBefore, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	originalHook := afterWakeRetireArtifactSnapshot
	afterWakeRetireArtifactSnapshot = func() {
		if err := installReplacement(); err != nil {
			t.Errorf("install replacement state: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireArtifactSnapshot = originalHook })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, wakeTargetFileName) ||
		!strings.Contains(result.Reason, wakeStateFileName) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target changed after state replacement: %v", err)
	}
	stateAfter, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(stateBefore, stateAfter) {
		t.Fatal("state replacement did not change file identity")
	}
	assertFileRawForTest(t, statePath, stateRaw)
}

func TestRetireWakePreservesCoupledStateReplacementInstalledAfterCommit(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	stateRaw, installReplacement := stageExactWakeArtifactReplacement(t, statePath, ".exact-cleanup-refresh")
	originalHook := afterWakeRetireLockRemoval
	afterWakeRetireLockRemoval = func() {
		if err := installReplacement(); err != nil {
			t.Errorf("refresh exact state projection: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireLockRemoval = originalHook })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target was removed with replacement state: %v", err)
	}
	assertFileRawForTest(t, statePath, stateRaw)
}

func TestRetireWakeUnboundMalformedStateIsPreservedAsResidue(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := fixture.created.Lock
	lock.StateGeneration = ""
	lock.StateDigest = ""
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	malformed := []byte("{not-json\n")
	if err := os.WriteFile(statePath, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, wakeStateFileName) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock"))
	assertPathMissingForTest(t, wakeTargetPath(fixture.root, fixture.me))
	assertFileRawForTest(t, statePath, malformed)
}

func TestRetireWakeBoundMalformedStateRefusesBeforeCommit(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	if err := os.WriteFile(statePath, []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	before := snapshotWakeCheckTree(t, fixture.root)

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err == nil || result.Status != "refused" {
		t.Fatalf("result=%#v err=%v, want bound-state refusal", result, err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestRetireWakeStateUnlinkFailureIsSuccessfulResidue(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	originalUnlink := wakeRetireUnlinkStateAt
	wakeRetireUnlinkStateAt = func(dirfd int, name string, flags int) error {
		if name == wakeStateFileName {
			return syscall.EIO
		}
		return originalUnlink(dirfd, name, flags)
	}
	t.Cleanup(func() { wakeRetireUnlinkStateAt = originalUnlink })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, wakeStateFileName) ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock"))
	assertPathMissingForTest(t, wakeTargetPath(fixture.root, fixture.me))
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)); err != nil {
		t.Fatalf("state residue missing after injected unlink failure: %v", err)
	}
}

func TestRetireWakeStaleLockFsyncFailureSkipsArtifactCleanup(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	originalSync := syncWakeLockAfterCommitDirFD
	syncWakeLockAfterCommitDirFD = func(int) error { return syscall.EIO }
	t.Cleanup(func() { syncWakeLockAfterCommitDirFD = originalSync })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, "wake lock durability") ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock"))
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target cleanup was not skipped after lock fsync failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)); err != nil {
		t.Fatalf("state cleanup was not skipped after lock fsync failure: %v", err)
	}
}

func TestRetireWakeStaleLockPreUnlinkFailureIsRefused(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	targetRaw, err := os.ReadFile(wakeTargetPath(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	originalUnlink := wakeRetireUnlinkWakeLockAt
	wakeRetireUnlinkWakeLockAt = func(dirfd int, name string, flags int) error {
		if name == ".wake.lock" {
			return syscall.EPERM
		}
		return originalUnlink(dirfd, name, flags)
	}
	t.Cleanup(func() { wakeRetireUnlinkWakeLockAt = originalUnlink })

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "remove stale wake lock") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock changed after pre-unlink refusal: %v", err)
	}
	assertFileRawForTest(t, wakeTargetPath(root, "codex"), targetRaw)
}

func TestRetireWakeUnboundStateWithDifferentTargetDigestIsPreserved(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := fixture.created.Lock
	lock.StateGeneration = ""
	lock.StateDigest = ""
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeWakeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	state.Target.TargetDigest = "sha256:" + strings.Repeat("0", 64)
	replacementRaw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, replacementRaw) {
		t.Fatal("state target digest mutation did not change bytes")
	}
	if err := os.WriteFile(statePath, replacementRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock"))
	assertPathMissingForTest(t, wakeTargetPath(fixture.root, fixture.me))
	assertFileRawForTest(t, statePath, replacementRaw)
}

func TestRetireWakeRefusesBoundStateSnapshotAmbiguityBeforeLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := os.Remove(filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "bound wake state") {
		t.Fatalf("result=%#v err=%v, want bound-state refusal", result, err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
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

func installRetireAgentDirectoryReplacement(
	t *testing.T,
	root string,
	me string,
	names ...string,
) (detachedPath string, successorLockSnapshot func() os.FileInfo) {
	t.Helper()
	agentPath := fsq.AgentBase(root, me)
	detachedPath = agentPath + ".retire-detached"
	var successorLock os.FileInfo
	originalHook := afterWakeRetireValidation
	afterWakeRetireValidation = func() {
		afterWakeRetireValidation = func() {}
		if err := os.Rename(agentPath, detachedPath); err != nil {
			t.Fatalf("detach retiring agent directory: %v", err)
		}
		if err := os.Mkdir(agentPath, 0o700); err != nil {
			t.Fatalf("create replacement agent directory: %v", err)
		}
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(detachedPath, name))
			if err != nil {
				t.Fatalf("read detached %s: %v", name, err)
			}
			info, err := os.Stat(filepath.Join(detachedPath, name))
			if err != nil {
				t.Fatalf("stat detached %s: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(agentPath, name), raw, info.Mode().Perm()); err != nil {
				t.Fatalf("copy replacement %s: %v", name, err)
			}
		}
		var err error
		successorLock, err = os.Stat(filepath.Join(agentPath, ".wake.lock"))
		if err != nil {
			t.Fatalf("stat replacement wake lock: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireValidation = originalHook })
	return detachedPath, func() os.FileInfo { return successorLock }
}

func matchingRetireWakeProcessFromLock(pid int, lock wakeLock) wakeProcessInfo {
	return wakeProcessInfo{
		PID:        pid,
		Running:    true,
		StartToken: lock.ProcessStart,
		BootID:     lock.BootID,
		Executable: lock.Executable,
		Args:       append([]string(nil), lock.Args...),
	}
}

func stageExactWakeArtifactReplacement(
	t *testing.T,
	path string,
	suffix string,
) ([]byte, func() error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := path + suffix
	if err := os.WriteFile(replacementPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw, func() error { return os.Rename(replacementPath, path) }
}

func assertRetireReplacementLockPreserved(
	t *testing.T,
	root string,
	me string,
	successorBefore os.FileInfo,
) {
	t.Helper()
	if successorBefore == nil {
		t.Fatal("retirement never reached retained-directory replacement seam")
	}
	successorAfter, err := os.Stat(filepath.Join(fsq.AgentBase(root, me), ".wake.lock"))
	if err != nil {
		t.Fatalf("replacement wake lock was removed: %v", err)
	}
	if !sameWakeFileIdentity(successorBefore, successorAfter) {
		t.Fatal("replacement wake lock identity changed")
	}
}
