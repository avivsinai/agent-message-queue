//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundAuthoritativeWakeReleaseRefusesInconclusiveState(t *testing.T) {
	tests := map[string]func(*testing.T, *authoritativeWakePreparedCleanupFixture){
		"missing": func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			if err := os.Remove(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
				t.Fatal(err)
			}
		},
		"noncanonical": func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"target_digest_mismatch": func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state wakeState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state.Target.TargetDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			raw, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"partial_lock_binding": func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			lock := fixture.inspection.Lock
			lock.StateDigest = ""
			replaceWakeLockForTest(t, fixture.lockPath, lock)
			fixture.inspection = inspectWakeLock(fixture.root, fixture.me)
		},
		"newer_document": func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			installNewerWakeStateSchemaForTest(t, filepath.Join(fixture.agentDir.path, wakeStateFileName), "document")
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			mutate(t, fixture)
			before := snapshotWakeCheckTree(t, fixture.root)
			err := fixture.release()
			var inconclusive *wakeStateBoundInconclusiveError
			if !errors.As(err, &inconclusive) {
				t.Fatalf("release error = %v, want bound inconclusive", err)
			}
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
		})
	}
}

func TestBoundAuthoritativeWakeReleasePreservesReplacementDuringStateValidation(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	original := afterWakeStateBoundSelection
	var replacedTree map[string]wakeCheckTreeEntry
	afterWakeStateBoundSelection = func() {
		afterWakeStateBoundSelection = func() {}
		replaceWakeLockForTest(t, fixture.lockPath, fixture.inspection.Lock)
		replacedTree = snapshotWakeCheckTree(t, fixture.root)
	}
	t.Cleanup(func() { afterWakeStateBoundSelection = original })

	err := fixture.release()
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("release error = %v, want bound inconclusive", err)
	}
	if replacedTree == nil {
		t.Fatal("lock replacement seam did not run")
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, replacedTree)
}

func TestBoundGenericWakeOwnerlessMutationRefusesInconclusiveState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(fixture.root, fixture.me)
	before := snapshotWakeCheckTree(t, fixture.root)
	err := validateWakeLockOwnerlessMutation(inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("ownerless mutation error = %v, want bound inconclusive", err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestBoundGenericWakeCleanupRefusesInconclusiveState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)
	err := fixture.cleanupNow()
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("generic cleanup error = %v, want bound inconclusive", err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestTargetlessAcquisitionPreservesNoLockTargetShadow(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)
	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{
		wakeMode: wakeInjectModeNone,
	})
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	if err == nil {
		t.Fatal("targetless acquisition removed a no-lock target shadow")
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}
