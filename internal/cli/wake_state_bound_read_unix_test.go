//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestBoundWakeStateSelectionFailsClosedWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, root string, inspection *wakeLockInspection)
		changed bool
	}{
		{name: "valid"},
		{name: "missing state", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			if err := os.Remove(filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "corrupt state", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "noncanonical state", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			path := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "newer state", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			path := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, newerWakeStateRawForTest(t, raw, "document"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "newer target", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			path := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, newerWakeStateRawForTest(t, raw, "target"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "missing target", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			if err := os.Remove(wakeTargetPath(root, "codex")); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "target digest mismatch", mutate: func(t *testing.T, root string, _ *wakeLockInspection) {
			target, exists, err := readWakeTarget(root, "codex")
			if err != nil || !exists {
				t.Fatalf("target exists=%v err=%v", exists, err)
			}
			target.Created = "2026-08-02T00:00:00Z"
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "prepared existence mismatch", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			if err := writeWakeGenerationFile(wakePreparedPath(root, "codex"), "wake prepared marker", wakeReady{
				Schema: wakeReadySchema, Generation: inspection.Lock.Generation, TargetDigest: inspection.Lock.TargetDigest,
			}); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "newer prepared", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			if err := writeWakePreparedFile(root, "codex", *inspection); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, newerWakeStateRawForTest(t, raw, "prepared"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "prepared digest mismatch", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			if err := writeWakePreparedFile(root, "codex", *inspection); err != nil {
				t.Fatal(err)
			}
			if err := writeWakeGenerationFile(wakePreparedPath(root, "codex"), "wake prepared marker", wakeReady{
				Schema: wakeReadySchema, Generation: inspection.Lock.Generation,
				TargetDigest: "sha256:" + strings.Repeat("0", 64),
			}); err != nil {
				t.Fatal(err)
			}
		}, changed: true},
		{name: "lock state generation mismatch", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			lock := inspection.Lock
			lock.StateGeneration = "11111111111111111111111111111111"
			writeBoundReadLockForTest(t, root, lock)
			*inspection = inspectWakeLock(root, "codex")
		}, changed: true},
		{name: "lock state digest mismatch", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			lock := inspection.Lock
			lock.StateDigest = "sha256:" + strings.Repeat("0", 64)
			writeBoundReadLockForTest(t, root, lock)
			*inspection = inspectWakeLock(root, "codex")
		}, changed: true},
		{name: "partial lock binding", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			lock := inspection.Lock
			lock.StateDigest = ""
			writeBoundReadLockForTest(t, root, lock)
			*inspection = inspectWakeLock(root, "codex")
		}, changed: true},
		{name: "null lock binding", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			raw := wakeBoundReadLockRawWithFieldForTest(t, inspection.Lock, "state_generation", json.RawMessage("null"))
			writeBoundReadLockRawForTest(t, root, raw)
			*inspection = inspectWakeLock(root, "codex")
		}, changed: true},
		{name: "wrong-type lock binding", mutate: func(t *testing.T, root string, inspection *wakeLockInspection) {
			raw := wakeBoundReadLockRawWithFieldForTest(t, inspection.Lock, "state_digest", json.RawMessage("123"))
			writeBoundReadLockRawForTest(t, root, raw)
			*inspection = inspectWakeLock(root, "codex")
		}, changed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, target, _ := newOwnerAcquisitionPublicationFixture(t)
			cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			inspection := inspectWakeLock(root, "codex")
			if test.mutate != nil {
				test.mutate(t, root, &inspection)
			}
			before := snapshotWakeCheckTree(t, root)

			selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
			if !test.changed {
				if err != nil || !selection.StatePreferred || !sameWakeTarget(selection.Target, target) {
					t.Fatalf("bound selection=%#v err=%v", selection, err)
				}
				return
			}
			var inconclusive *wakeStateBoundInconclusiveError
			if !errors.As(err, &inconclusive) || selection.TargetPresent || selection.PreparedPresent {
				t.Fatalf("bound failure selection=%#v err=%v", selection, err)
			}
			assertWakeCheckTreeUnchanged(t, root, before)
		})
	}
}

func TestBoundWakeStateSelectionWrapsDirectoryObservationFailures(t *testing.T) {
	tests := []struct {
		name    string
		install func(error)
	}{
		{
			name: "non-ENOENT open failure",
			install: func(failure error) {
				openWakeStateInspectionDirectory = func(string, string) (*wakeAgentDir, error) {
					return nil, failure
				}
			},
		},
		{
			name: "withFD failure",
			install: func(failure error) {
				withWakeStateInspectionDirectoryFD = func(*wakeAgentDir, func(int) error) error {
					return failure
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, target, _ := newOwnerAcquisitionPublicationFixture(t)
			cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &target, wakeMode: wakeTargetInjectVia,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			inspection := inspectWakeLock(root, "codex")

			originalOpen := openWakeStateInspectionDirectory
			originalWithFD := withWakeStateInspectionDirectoryFD
			failure := os.ErrPermission
			test.install(failure)
			t.Cleanup(func() {
				openWakeStateInspectionDirectory = originalOpen
				withWakeStateInspectionDirectoryFD = originalWithFD
			})

			selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
			var inconclusive *wakeStateBoundInconclusiveError
			if !errors.As(err, &inconclusive) || !errors.Is(err, failure) || errors.Is(err, os.ErrNotExist) ||
				selection.TargetPresent || selection.PreparedPresent {
				t.Fatalf("selection=%#v err=%v, want bound inconclusive wrapping observation failure", selection, err)
			}
		})
	}
}

func TestWakeStateBoundInconclusiveErrorConstructorIsIdempotent(t *testing.T) {
	first := newWakeStateBoundInconclusiveError(os.ErrPermission)
	second := newWakeStateBoundInconclusiveError(first)
	if second != first {
		t.Fatalf("second wrap = %T %v, want original error identity", second, second)
	}
	if got := second.Error(); got != os.ErrPermission.Error() {
		t.Fatalf("second wrap message = %q, want %q", got, os.ErrPermission)
	}
}

func TestBoundWakeStateSelectionPreservesSnapshotRaceCause(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")

	original := afterWakeStateDualReadDocument
	afterWakeStateDualReadDocument = func() {
		afterWakeStateDualReadDocument = func() {}
		changed := target
		changed.Created = "2026-08-02T00:00:00Z"
		if err := writeWakeTarget(root, "codex", changed); err != nil {
			t.Errorf("replace target: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateDualReadDocument = original })

	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &inconclusive) || !errors.As(err, &changed) || selection.TargetPresent || selection.PreparedPresent {
		t.Fatalf("selection=%#v err=%v, want bound error wrapping snapshot change", selection, err)
	}
}

func TestBoundWakeStateSelectionPreservesStateSnapshotRaceCause(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")
	statePath := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := statePath + ".replacement"
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	original := afterWakeStateSnapshotRead
	afterWakeStateSnapshotRead = func() {
		afterWakeStateSnapshotRead = func() {}
		if err := os.Rename(replacement, statePath); err != nil {
			t.Errorf("replace state: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateSnapshotRead = original })

	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &inconclusive) || !errors.As(err, &changed) || selection.TargetPresent || selection.PreparedPresent {
		t.Fatalf("selection=%#v err=%v, want bound error wrapping state snapshot change", selection, err)
	}
}

func TestBoundWakeStateSelectionRejectsLockReplacement(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")
	replacement := inspection.Lock
	replacement.Generation = "11111111111111111111111111111111"
	replacement.StateGeneration = replacement.Generation
	var afterReplacement map[string]wakeCheckTreeEntry
	original := afterWakeStateBoundSelection
	afterWakeStateBoundSelection = func() {
		afterWakeStateBoundSelection = func() {}
		writeBoundReadLockForTest(t, root, replacement)
		afterReplacement = snapshotWakeCheckTree(t, root)
	}
	t.Cleanup(func() { afterWakeStateBoundSelection = original })

	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &inconclusive) || !errors.As(err, &changed) || selection.TargetPresent || selection.PreparedPresent {
		t.Fatalf("selection=%#v err=%v, want bound lock snapshot change", selection, err)
	}
	if afterReplacement == nil {
		t.Fatal("lock replacement seam did not run")
	}
	assertWakeCheckTreeUnchanged(t, root, afterReplacement)
	if _, err := os.Stat(wakePreparedPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("bound lock replacement created prepared marker: %v", err)
	}
}

func TestBoundWakeStateSelectionRejectsRepublishedTargetDigest(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")
	changed := target
	changed.Created = "2026-08-02T00:00:00Z"
	if err := writeWakeTarget(root, "codex", changed); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		expected, err := captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, "codex")
		if err != nil {
			return err
		}
		_, err = publishWakeStateAt(dirfd, agentDir, root, "codex", expected)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	legacySelection, err := readWakeStateSelection(root, "codex")
	if err != nil || !legacySelection.StatePreferred || !sameWakeTarget(legacySelection.Target, changed) {
		t.Fatalf("republished selection=%#v err=%v", legacySelection, err)
	}
	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	var mismatch *wakeStateLegacyMismatchError
	if !errors.As(err, &inconclusive) || errors.As(err, &mismatch) || selection.TargetPresent || selection.PreparedPresent {
		t.Fatalf("selection=%#v err=%v, want bound lock-state digest refusal", selection, err)
	}
}

func TestBoundWakeStateFailureRefusesReadConsumers(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	target.Owner = nil
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")
	if err := os.Remove(filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, root)

	observation, err := observeWakeCheck(root, "codex")
	var checkInconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &checkInconclusive) || observation.Target.Exists || observation.Repair.TargetPresent || observation.Repair.RepairAvailable {
		t.Fatalf("wake check observation=%#v err=%v", observation, err)
	}
	if ready, err := validateWakePreparedFileAgainstInspection(root, "codex", inspection); err == nil || ready {
		t.Fatalf("prepared validation ready=%v err=%v, want bound refusal", ready, err)
	}
	if err := writeWakePreparedFile(root, "codex", inspection); err == nil {
		t.Fatal("prepared publication accepted a missing bound state")
	}
	if _, err := os.Stat(wakePreparedPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("prepared publication changed marker after bound refusal: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := agentDir.withFD(func(dirfd int) error {
		return validateWakeReadyLockAndTargetForInspectionAt(dirfd, agentDir, root, "codex", inspection, wakeReady{
			Schema: wakeReadySchema, Generation: inspection.Lock.Generation, TargetDigest: inspection.Lock.TargetDigest,
		})
	}); err == nil {
		t.Fatal("readiness validation accepted a missing bound state")
	}
	locks, _ := checkWakeLocksWithHintsSchema(root, []string{"codex"}, true, wakeCheckSchemaV2)
	if len(locks) != 1 || locks[0].TargetPresent || locks[0].RepairAvailable {
		t.Fatalf("doctor repair observation=%#v, want no usable target or repair", locks)
	}
	inspection.Status = wakeLockValid
	inspection.IdentityConfirmed = true
	inspection.Process = wakeProcessInfo{}
	inspection.Lock.TTY = "test-tty"
	if err := requireWakeLockUsable(inspection, wakeTargetInjectVia, &target); err == nil {
		t.Fatal("existing-wake reuse accepted a missing bound state")
	} else {
		var inconclusive *wakeStateBoundInconclusiveError
		if !errors.As(err, &inconclusive) {
			t.Fatalf("existing-wake error=%v, want bound inconclusive", err)
		}
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestBoundWakeCheckInconclusiveIsUnverifiedAndRetryOnly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string)
		race   bool
	}{
		{name: "missing state", mutate: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "legacy mismatch", mutate: func(t *testing.T, root string) {
			target, exists, err := readWakeTarget(root, "codex")
			if err != nil || !exists {
				t.Fatalf("target exists=%v err=%v", exists, err)
			}
			target.Created = "2026-08-02T00:00:00Z"
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "state race", race: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, target, _ := newOwnerAcquisitionPublicationFixture(t)
			target.Owner = nil
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			if test.mutate != nil {
				test.mutate(t, root)
			}
			if test.race {
				statePath := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
				raw, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatal(err)
				}
				replacement := statePath + ".replacement"
				if err := os.WriteFile(replacement, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				original := afterWakeStateSnapshotRead
				afterWakeStateSnapshotRead = func() {
					afterWakeStateSnapshotRead = func() {}
					if err := os.Rename(replacement, statePath); err != nil {
						t.Errorf("replace state: %v", err)
					}
				}
				t.Cleanup(func() { afterWakeStateSnapshotRead = original })
			}

			snapshot := inspectWakeCheckSnapshot(root, "codex")
			if snapshot.Decision.Wake.Status != string(wakeLockUnverified) ||
				snapshot.Decision.Wake.Live || snapshot.Decision.Repair.InjectViaAvailable ||
				snapshot.Decision.Action.Kind != wakeActionRetryCheck ||
				snapshot.OpsLock == nil || snapshot.OpsLock.TargetPresent || snapshot.OpsLock.RepairAvailable {
				t.Fatalf("public bound-inconclusive decision=%#v ops=%#v", snapshot.Decision, snapshot.OpsLock)
			}
			if _, err := os.Stat(wakePreparedPath(root, "codex")); !os.IsNotExist(err) {
				t.Fatalf("public wake check created prepared marker: %v", err)
			}
		})
	}
}

func TestUnboundWakeStateSelectionRetainsP2aFallback(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")
	lock := inspection.Lock
	lock.StateGeneration, lock.StateDigest = "", ""
	writeBoundReadLockForTest(t, root, lock)
	inspection = inspectWakeLock(root, "codex")
	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	if err != nil || !selection.StatePreferred || !sameWakeTarget(selection.Target, target) {
		t.Fatalf("unbound selection=%#v err=%v", selection, err)
	}
}

func TestTargetlessWakeLockRemainsUnbound(t *testing.T) {
	lock := wakeLock{
		Generation: "11111111111111111111111111111111",
		WakeMode:   wakeInjectModeNone,
	}
	raw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := wakeLockInspectionStateBound(wakeLockInspection{
		Exists: true,
		Lock:   lock,
		raw:    raw,
	})
	if err != nil || bound {
		t.Fatalf("targetless state binding bound=%v err=%v", bound, err)
	}
}

func writeBoundReadLockForTest(t *testing.T, root string, lock wakeLock) {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	writeWakeLockExactForTest(t, root, "codex", lock)
	if err := os.Chmod(path, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
}

func wakeBoundReadLockRawWithFieldForTest(
	t *testing.T,
	lock wakeLock,
	name string,
	value json.RawMessage,
) []byte {
	t.Helper()
	raw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields[name] = value
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeBoundReadLockRawForTest(t *testing.T, root string, raw []byte) {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
}
