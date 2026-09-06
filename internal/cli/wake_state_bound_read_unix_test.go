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
