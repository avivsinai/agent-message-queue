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

	err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return removeWakeLockIfUnchangedGuardedAt(scope, inspection)
	})
	assertDetachedWakeCleanupOnlyError(t, err)
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
