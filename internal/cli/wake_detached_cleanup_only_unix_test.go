//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
