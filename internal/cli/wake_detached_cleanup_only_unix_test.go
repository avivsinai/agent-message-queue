//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDetachedBoundWakeResidueCleanupReturnsCleanupOnlyError(t *testing.T) {
	fixture, inspection, detachedPath := newDetachedBoundGenericWakeResidue(t)
	successorBefore := snapshotDetachedWakeFiles(t, fixture.agentDir.path, ".wake.lock", wakeTargetFileName, wakeStateFileName, wakePreparedFileName)
	residueBefore := snapshotDetachedWakeFiles(t, detachedPath, wakeTargetFileName, wakePreparedFileName)

	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, inspection)
	})
	assertDetachedWakeCleanupOnlyError(t, err)
	assertDetachedBoundWakeResidueRemoved(t, detachedPath, residueBefore)
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestWakeLockRemovalClassifiesDirectorySwapBeforeUnlink(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	var detachedPath string
	var successorBefore map[string]detachedWakeFileSnapshot
	originalAfterRead := afterWakeLockAtRead
	afterWakeLockAtRead = func() {
		afterWakeLockAtRead = func() {}
		detachedPath = detachGenericWakeAgentDirForTest(
			t,
			fixture.agentDir.path,
			".wake.lock",
			wakePreparedFileName,
			wakeLifecycleGuardFileName,
		)
		successorBefore = snapshotDetachedWakeFiles(
			t,
			fixture.agentDir.path,
			".wake.lock",
			wakePreparedFileName,
			wakeLifecycleGuardFileName,
		)
	}
	t.Cleanup(func() { afterWakeLockAtRead = originalAfterRead })

	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, fixture.created)
	})
	if detachedPath == "" {
		t.Fatal("directory swap did not run between the initial sample and unlink")
	}
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("directory-swap cleanup error = %v, want detached cleanup-only", err)
	}
	if err == nil {
		t.Fatal("directory-swap cleanup reported canonical success")
	}
	assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestWakeLockRemovalClassifiesDirectorySwapAfterUnlink(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	var detachedPath string
	var successorBefore map[string]detachedWakeFileSnapshot
	var outcome wakeLockRemovalOutcome
	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		outcome = removeWakeLockIfUnchangedGuardedAtOutcome(
			dirfd,
			fixture.agentDir,
			fixture.created,
			func() error {
				if err := unix.Unlinkat(dirfd, ".wake.lock", 0); err != nil {
					return err
				}
				detachedPath = detachGenericWakeAgentDirForTest(
					t,
					fixture.agentDir.path,
					wakePreparedFileName,
					wakeLifecycleGuardFileName,
				)
				successorBefore = snapshotDetachedWakeFiles(
					t,
					fixture.agentDir.path,
					wakePreparedFileName,
					wakeLifecycleGuardFileName,
				)
				return nil
			},
		)
		return outcome.Err
	})
	if detachedPath == "" {
		t.Fatal("directory swap did not run after unlink")
	}
	if !outcome.Committed {
		t.Fatalf("directory-swap removal committed=%v err=%v, want committed cleanup", outcome.Committed, outcome.Err)
	}
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(outcome.Err, &cleanupOnly) {
		t.Fatalf("post-unlink directory-swap error = %v, want detached cleanup-only", outcome.Err)
	}
	if err == nil {
		t.Fatal("post-unlink directory swap reported canonical success")
	}
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestDetachedBoundWakeResidueCleanupFailsClosedWhenCanonicalPathAbsent(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true}
	})
	inspection := inspectWakeLock(fixture.root, fixture.me)
	detachedPath := fixture.agentDir.path + ".detached-absent-canonical"
	if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
		t.Fatal(err)
	}
	residueBefore := snapshotDetachedWakeFiles(
		t,
		detachedPath,
		".wake.lock",
		wakeTargetFileName,
		wakeStateFileName,
		wakePreparedFileName,
	)

	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, inspection)
	})
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if errors.As(err, &cleanupOnly) {
		t.Fatalf("canonical-path-absent cleanup returned detached authority: %v", err)
	}
	var bound *wakeStateBoundInconclusiveError
	if !errors.As(err, &bound) {
		t.Fatalf("canonical-path-absent cleanup error = %v, want bound inconclusive", err)
	}
	assertDetachedWakeFilesUnchanged(t, detachedPath, residueBefore)
}

func TestDetachedBoundGenericWakeAcquireStopsWithoutPublishing(t *testing.T) {
	fixture, _, detachedPath := newDetachedBoundGenericWakeResidue(t)
	successorBefore := snapshotDetachedWakeFiles(t, fixture.agentDir.path, ".wake.lock", wakeTargetFileName, wakeStateFileName, wakePreparedFileName)
	residueBefore := snapshotDetachedWakeFiles(t, detachedPath, wakeTargetFileName, wakePreparedFileName)

	cleanup, err := acquireWakeLockWithOptionsInDir(
		fixture.agentDir,
		fixture.root,
		fixture.me,
		fixture.options,
	)
	if cleanup != nil {
		t.Errorf("detached generic acquisition returned cleanup after refusal")
	}
	assertDetachedWakeCleanupOnlyError(t, err)
	assertDetachedBoundWakeResidueRemoved(t, detachedPath, residueBefore)
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestDetachedBoundStaleGenericWakeAcquireStopsWithoutPublishing(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	assertDetached := installDetachedBoundGenericWakeAfterSelection(
		t,
		fixture,
		".wake.lock",
		wakeTargetFileName,
		wakePreparedFileName,
	)
	cleanup, err := acquireWakeLockWithOptionsInDir(
		fixture.agentDir,
		fixture.root,
		fixture.me,
		fixture.options,
	)
	if cleanup != nil {
		t.Errorf("detached stale generic acquisition returned cleanup after refusal")
	}
	detachedPath, successorBefore, residueBefore := assertDetached()
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if errors.As(err, &cleanupOnly) {
		t.Fatalf("stale acquisition error = %v, want failure before detached cleanup", err)
	}
	var bound *wakeStateBoundInconclusiveError
	if !errors.As(err, &bound) {
		t.Fatalf("stale acquisition error = %v, want bound inconclusive", err)
	}
	assertDetachedWakeStateMissing(t, detachedPath)
	assertDetachedWakeFilesUnchanged(t, detachedPath, residueBefore)
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestDetachedBoundValidWakeReuseUsesRetainedState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	lock := fixture.created.Lock
	lock.PID = 4242
	lock.ProcessStart = "wake-start"
	lock.BootID = "boot-1"
	lock.Executable = "/usr/local/bin/amq"
	lock.Args = []string{"/usr/local/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", fixture.target.InjectVia}
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcessFromLock(pid, lock)
	})
	detachedPath := fixture.agentDir.path + ".valid-reuse-detached"
	var successorBefore map[string]detachedWakeFileSnapshot
	originalAfterRead := afterWakeLockAtRead
	afterWakeLockAtRead = func() {
		afterWakeLockAtRead = func() {}
		if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
			t.Fatalf("detach valid wake agent directory: %v", err)
		}
		if err := os.Mkdir(fixture.agentDir.path, 0o700); err != nil {
			t.Fatalf("create valid wake successor directory: %v", err)
		}
		copyDetachedWakeSuccessorFiles(t, detachedPath, fixture.agentDir.path)
		if err := os.Remove(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
			t.Fatalf("remove successor wake state: %v", err)
		}
		successorBefore = snapshotDetachedWakeFiles(
			t,
			fixture.agentDir.path,
			".wake.lock",
			wakeTargetFileName,
			wakePreparedFileName,
		)
	}
	t.Cleanup(func() { afterWakeLockAtRead = originalAfterRead })

	cleanup, err := acquireWakeLockWithOptionsInDir(
		fixture.agentDir,
		fixture.root,
		fixture.me,
		wakeLockAcquireOptions{
			acceptExistingValid: true,
			target:              fixture.target,
			wakeMode:            wakeTargetInjectVia,
		},
	)
	if cleanup != nil {
		t.Fatal("detached valid-wake reuse returned cleanup authority")
	}
	var alreadyRunning *wakeAlreadyRunningError
	if !errors.As(err, &alreadyRunning) {
		t.Fatalf("detached valid-wake reuse error = %v, want already running", err)
	}
	if successorBefore == nil {
		t.Fatal("valid-wake reuse did not reach detached-directory seam")
	}
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
	if _, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("missing successor state changed during retained reuse: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, wakeStateFileName)); err != nil {
		t.Fatalf("retained wake state was not preserved: %v", err)
	}
}

func TestDetachedBoundWakeRepairStopsWithoutStarting(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	writeWakeRepairFloorForTest(t, fixture.root, fixture.me, *fixture.target, nil)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	assertDetached := installDetachedBoundGenericWakeAfterSelection(
		t,
		fixture,
		wakeTargetFileName,
		wakePreparedFileName,
		wakeRepairFloorFileName,
	)
	started := false
	stubStartWakeFromTarget(t, func(string, string, wakeTarget, wakeRepairFloor) (int, error) {
		started = true
		return 0, errors.New("repair start must not run after detached cleanup")
	})

	result, err := repairWake(fixture.root, fixture.me)
	if started {
		t.Fatal("repair started through detached wake directory")
	}
	detachedPath, successorBefore, residueBefore := assertDetached()
	assertDetachedWakeCleanupOnlyError(t, err)
	if result.Status != "refused" {
		t.Fatalf("detached repair result = %#v, want refused", result)
	}
	if result.Reason != err.Error() {
		t.Fatalf("detached repair reason = %q, want returned error %q", result.Reason, err)
	}
	assertDetachedBoundWakeResidueRemoved(t, detachedPath, residueBefore)
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func newDetachedBoundGenericWakeResidue(
	t *testing.T,
) (*genericWakePreparedCleanupFixture, wakeLockInspection, string) {
	t.Helper()
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true}
	})
	inspection := inspectWakeLock(fixture.root, fixture.me)
	if inspection.Status != wakeLockUnverified || classifyPersistedWakeClaim(inspection) != wakeClaimGeneric {
		t.Fatalf("generic bound inspection = %#v", inspection)
	}

	detachedPath := detachBoundGenericWakeResidue(t, fixture)
	return fixture, inspection, detachedPath
}

func installDetachedBoundGenericWakeAfterSelection(
	t *testing.T,
	fixture *genericWakePreparedCleanupFixture,
	residueNames ...string,
) func() (string, map[string]detachedWakeFileSnapshot, map[string]detachedWakeFileSnapshot) {
	t.Helper()
	var detachedPath string
	var successorBefore map[string]detachedWakeFileSnapshot
	var residueBefore map[string]detachedWakeFileSnapshot
	original := afterWakeStateBoundSelection
	afterWakeStateBoundSelection = func() {
		afterWakeStateBoundSelection = func() {}
		detachedPath = detachBoundGenericWakeResidue(t, fixture)
		successorBefore = snapshotDetachedWakeFiles(
			t,
			fixture.agentDir.path,
			".wake.lock",
			wakeTargetFileName,
			wakeStateFileName,
			wakePreparedFileName,
		)
		residueBefore = snapshotDetachedWakeFiles(t, detachedPath, residueNames...)
	}
	t.Cleanup(func() { afterWakeStateBoundSelection = original })

	return func() (string, map[string]detachedWakeFileSnapshot, map[string]detachedWakeFileSnapshot) {
		if detachedPath == "" {
			t.Fatal("bound state selection did not reach detached-directory seam")
		}
		return detachedPath, successorBefore, residueBefore
	}
}

func detachBoundGenericWakeResidue(
	t *testing.T,
	fixture *genericWakePreparedCleanupFixture,
) string {
	t.Helper()
	detachedPath := fixture.agentDir.path + ".detached"
	if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
		t.Fatalf("detach wake agent directory: %v", err)
	}
	if err := os.Mkdir(fixture.agentDir.path, 0o700); err != nil {
		t.Fatalf("create successor wake agent directory: %v", err)
	}
	copyDetachedWakeSuccessorFiles(t, detachedPath, fixture.agentDir.path)
	if err := os.Remove(filepath.Join(detachedPath, wakeStateFileName)); err != nil {
		t.Fatalf("remove detached bound wake state: %v", err)
	}
	return detachedPath
}

func copyDetachedWakeSuccessorFiles(t *testing.T, from, to string) {
	t.Helper()
	for _, name := range []string{".wake.lock", wakeTargetFileName, wakeStateFileName, wakePreparedFileName} {
		fromPath := filepath.Join(from, name)
		raw, err := os.ReadFile(fromPath)
		if err != nil {
			t.Fatalf("read successor %s: %v", name, err)
		}
		info, err := os.Stat(fromPath)
		if err != nil {
			t.Fatalf("stat successor %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(to, name), raw, info.Mode().Perm()); err != nil {
			t.Fatalf("write successor %s: %v", name, err)
		}
	}
}

func assertDetachedWakeCleanupOnlyError(t *testing.T, err error) {
	t.Helper()
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("detached cleanup error = %v, want wakeDetachedCleanupOnlyError", err)
	}
	var bound *wakeStateBoundInconclusiveError
	if !errors.As(err, &bound) {
		t.Fatalf("detached cleanup error = %v, want wrapped bound validation error", err)
	}
}

func assertDetachedBoundWakeResidueRemoved(
	t *testing.T,
	detachedPath string,
	before map[string]detachedWakeFileSnapshot,
) {
	t.Helper()
	for _, name := range []string{".wake.lock", wakeStateFileName} {
		if _, err := os.Stat(filepath.Join(detachedPath, name)); !os.IsNotExist(err) {
			t.Fatalf("detached wake residue %s = %v, want absent", name, err)
		}
	}
	assertDetachedWakeFilesUnchanged(t, detachedPath, before)
}

func assertDetachedWakeStateMissing(t *testing.T, detachedPath string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(detachedPath, wakeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("detached wake state = %v, want absent", err)
	}
}

type detachedWakeFileSnapshot struct {
	raw  []byte
	info os.FileInfo
}

func snapshotDetachedWakeFiles(
	t *testing.T,
	dir string,
	names ...string,
) map[string]detachedWakeFileSnapshot {
	t.Helper()
	snapshot := make(map[string]detachedWakeFileSnapshot, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read wake file %s: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat wake file %s: %v", path, err)
		}
		snapshot[name] = detachedWakeFileSnapshot{raw: raw, info: info}
	}
	return snapshot
}

func assertDetachedWakeFilesUnchanged(
	t *testing.T,
	dir string,
	before map[string]detachedWakeFileSnapshot,
) {
	t.Helper()
	for name, snapshot := range before {
		path := filepath.Join(dir, name)
		assertWakeFileSnapshotUnchangedForTest(t, path, snapshot.raw, snapshot.info)
	}
}

func detachGenericWakeAgentDirForTest(t *testing.T, path string, successorNames ...string) string {
	t.Helper()
	detachedPath := path + ".detached-generic"
	if err := os.Rename(path, detachedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range successorNames {
		fromPath := filepath.Join(detachedPath, name)
		raw, err := os.ReadFile(fromPath)
		if err != nil {
			t.Fatalf("read detached successor source %s: %v", name, err)
		}
		info, err := os.Stat(fromPath)
		if err != nil {
			t.Fatalf("stat detached successor source %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(path, name), raw, info.Mode().Perm()); err != nil {
			t.Fatalf("write detached successor %s: %v", name, err)
		}
	}
	return detachedPath
}
