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

func TestBoundAuthoritativeWakeReleaseReconcilesPreparedPublicationGap(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*wakeState)
	}{
		{
			name: "missing prepared projection",
			mutate: func(state *wakeState) {
				state.Prepared = nil
			},
		},
		{
			name: "stale prepared projection",
			mutate: func(state *wakeState) {
				state.Prepared.Generation = "00000000000000000000000000000000"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			installWakeStateMutationForTest(t, statePath, test.mutate)

			if err := fixture.release(); err != nil {
				t.Fatalf("release after prepared publication gap: %v", err)
			}
			fixture.assertReleasedClaimMissing(t)
			assertPathMissingForTest(t, fixture.preparedPath)
			fixture.assertControlSocketMissing(t)
		})
	}
}

func TestBoundPreparedPublicationGapReconciliationRefusesMarkerMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*wakeReady)
	}{
		{
			name: "generation",
			mutate: func(marker *wakeReady) {
				marker.Generation = "00000000000000000000000000000000"
			},
		},
		{
			name: "target digest",
			mutate: func(marker *wakeReady) {
				marker.TargetDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			installWakeStateMutationForTest(t, statePath, func(state *wakeState) {
				state.Prepared = nil
			})
			marker := fixture.preparedMarker
			test.mutate(&marker)
			writeAuthoritativePreparedMarkerForTest(t, fixture.preparedPath, marker)
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

func TestBoundPreparedPublicationGapAllowsPublicWakeRestart(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	installWakeStateMutationForTest(t, statePath, func(state *wakeState) {
		state.Prepared = nil
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, fixture.options)
	if err != nil {
		t.Fatalf("restart after prepared publication gap: %v", err)
	}
	t.Cleanup(cleanup)
	restarted := inspectWakeLock(fixture.root, fixture.me)
	if sameWakeLockGeneration(fixture.created, restarted) {
		t.Fatal("restart retained the stale wake generation")
	}
	if err := writeWakePreparedFile(fixture.root, fixture.me, restarted); err != nil {
		t.Fatalf("publish restarted wake preparation: %v", err)
	}
	state := readWakeStateAtPathForTest(t, fixture.root, fixture.me)
	if state.State.Prepared == nil ||
		state.State.Prepared.Generation != restarted.Lock.Generation ||
		state.State.Prepared.TargetDigest != restarted.Lock.TargetDigest {
		t.Fatalf("restarted prepared projection = %#v, lock = %#v", state.State.Prepared, restarted.Lock)
	}
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

func TestTargetlessAcquisitionQuarantinesNoLockTargetShadow(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	targetPath := wakeTargetPath(fixture.root, fixture.me)
	targetRaw, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{
		wakeMode: wakeInjectModeNone,
	})
	if err != nil {
		t.Fatalf("targetless acquisition after orphan quarantine: %v", err)
	}
	t.Cleanup(cleanup)

	if _, err := os.Lstat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("orphan target still occupies live path: %v", err)
	}
	assertExactWakeQuarantineForTest(
		t,
		fixture.agentDir.path,
		".wake.target.quarantined.",
		targetRaw,
		targetInfo,
	)
	inspection := inspectWakeLock(fixture.root, fixture.me)
	if !inspection.Exists || inspection.Lock.PID != os.Getpid() || inspection.Lock.TargetDigest != "" ||
		inspection.Lock.StateGeneration != "" || inspection.Lock.StateDigest != "" {
		t.Fatalf("fresh targetless wake = %#v, want new unbound lock", inspection)
	}
}

func installWakeStateMutationForTest(t *testing.T, path string, mutate func(*wakeState)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeWakeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&state)
	raw, err = encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
