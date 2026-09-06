//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthoritativeWakeAcquisitionPublishesLegacyFirstState(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	state := readWakeStateAtPathForTest(t, root, "codex")
	if !sameWakeTarget(state.State.Target.wakeTarget(), target) {
		t.Fatalf("state target = %#v, want %#v", state.State.Target, target)
	}
	if state.State.Prepared != nil {
		t.Fatalf("new acquisition state prepared = %#v, want nil", state.State.Prepared)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Lock.StateGeneration != inspection.Lock.Generation ||
		inspection.Lock.StateDigest != inspection.Lock.TargetDigest ||
		inspection.Lock.StateDigest != state.State.Target.TargetDigest {
		t.Fatalf("authoritative bound lock = %#v, state target = %#v", inspection.Lock, state.State.Target)
	}
}

func TestWakePreparedPublicationRefreshesStateLegacyFirst(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	inspection := inspectWakeLock(root, "codex")

	injected := errors.New("state refresh failed after prepared commit")
	installWakeStatePublicationFailure(t, wakeStateAfterTempWrite, injected)
	if preparedErr := writeWakePreparedFile(root, "codex", inspection); !errors.Is(preparedErr, injected) {
		t.Fatalf("prepared publication error = %v, want state refresh failure", preparedErr)
	}
	if _, exists, err := readWakeGenerationFile(wakePreparedPath(root, "codex"), "wake prepared marker"); err != nil || !exists {
		t.Fatalf("legacy prepared marker exists=%v err=%v", exists, err)
	}
	state := readWakeStateAtPathForTest(t, root, "codex")
	if state.State.Prepared != nil {
		t.Fatalf("failed refresh changed visible state prepared = %#v", state.State.Prepared)
	}

	afterWakeStatePublicationBoundary = func(wakeStatePublicationBoundary) error { return nil }
	if err := writeWakePreparedFile(root, "codex", inspection); err != nil {
		t.Fatal(err)
	}
	state = readWakeStateAtPathForTest(t, root, "codex")
	if state.State.Prepared == nil || state.State.Prepared.Generation != inspection.Lock.Generation {
		t.Fatalf("refreshed state prepared = %#v, want generation %q", state.State.Prepared, inspection.Lock.Generation)
	}
}

func TestRecoverOwnerWithoutTargetConvergesExactState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state missing before recovery retry: %v", err)
	}

	result, err := recoverOwnerWake(fixture.root, fixture.me)
	if err != nil || result.Status != "recovered" {
		t.Fatalf("targetless recovery = %#v err=%v", result, err)
	}
	assertPathMissingForTest(t, statePath)
}

func TestAuthoritativeWakeReleaseRemovesStateAfterTarget(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state missing before release: %v", err)
	}
	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, statePath)
}

func TestUnboundP2aPreparedMutationPreservesNewerWakeStateSchemas(t *testing.T) {
	for _, component := range []string{"document", "target", "prepared"} {
		t.Run(component, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			stateRaw, stateInfo := installNewerWakeStateSchemaForTest(t, statePath, component)

			var writeErr error
			stderr := captureWakeStderr(t, func() {
				writeErr = writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
			})
			if writeErr != nil {
				t.Fatalf("prepared mutation error = %v, want successful legacy commit", writeErr)
			}
			assertSingleWakeStateProjectionWarning(t, stderr)
			assertWakeStateSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
			marker, exists, err := readWakeGenerationFile(fixture.preparedPath, "wake prepared marker")
			if err != nil || !exists || marker.Generation != fixture.inspection.Lock.Generation {
				t.Fatalf("legacy prepared marker = %#v exists=%v err=%v", marker, exists, err)
			}
		})
	}
}

func unbindAuthoritativeWakePreparedFixtureForP2a(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
	t.Helper()
	lock := fixture.inspection.Lock
	lock.StateGeneration = ""
	lock.StateDigest = ""
	raw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.lockPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.lockPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	fixture.inspection = inspectWakeLock(fixture.root, fixture.me)
	if !fixture.inspection.Exists || fixture.inspection.Lock.StateGeneration != "" || fixture.inspection.Lock.StateDigest != "" {
		t.Fatalf("unbound P2a fixture = %#v", fixture.inspection)
	}
}

func readWakeStateAtPathForTest(t *testing.T, root, me string) wakeStateFileSnapshot {
	t.Helper()
	snapshot, exists := readOptionalWakeStateAtPathForTest(t, root, me)
	if !exists {
		t.Fatal("wake state is missing")
	}
	return snapshot
}

func readOptionalWakeStateAtPathForTest(t *testing.T, root, me string) (wakeStateFileSnapshot, bool) {
	t.Helper()
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	var snapshot wakeStateFileSnapshot
	var exists bool
	if err := agentDir.withFD(func(dirfd int) error {
		var readErr error
		snapshot, exists, readErr = readWakeStateSnapshotAt(dirfd, agentDir)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot, exists
}

func installWakeStatePublicationFailure(t *testing.T, boundary wakeStatePublicationBoundary, injected error) {
	t.Helper()
	original := afterWakeStatePublicationBoundary
	afterWakeStatePublicationBoundary = func(current wakeStatePublicationBoundary) error {
		if current == boundary {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { afterWakeStatePublicationBoundary = original })
}

func installNewerWakeStateSchemaForTest(t *testing.T, path, component string) ([]byte, os.FileInfo) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = newerWakeStateRawForTest(t, raw, component)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw, info
}

func newerWakeStateRawForTest(t *testing.T, raw []byte, component string) []byte {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	newerSchema, err := json.Marshal(wakeStateSchema + 1)
	if err != nil {
		t.Fatal(err)
	}
	if component == "document" {
		document["schema"] = newerSchema
		document["future_field"] = json.RawMessage("true")
	} else {
		var section map[string]json.RawMessage
		if err := json.Unmarshal(document[component], &section); err != nil {
			t.Fatal(err)
		}
		section["schema"] = newerSchema
		section["future_field"] = json.RawMessage("true")
		document[component], err = json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertSingleWakeStateProjectionWarning(t *testing.T, stderr string) {
	t.Helper()
	if count := strings.Count(stderr, "warning: wake state projection failed:"); count != 1 ||
		!strings.Contains(stderr, "newer schema") ||
		!strings.Contains(stderr, "continuing with legacy wake state") {
		t.Fatalf("projection warning count=%d stderr=%q", count, stderr)
	}
}

func assertWakeStateSnapshotUnchangedForTest(t *testing.T, path string, raw []byte, info os.FileInfo) {
	t.Helper()
	assertWakeFileSnapshotUnchangedForTest(t, path, raw, info)
}

func assertWakeFileSnapshotUnchangedForTest(t *testing.T, path string, raw []byte, info os.FileInfo) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("preserved wake file %s bytes=%q err=%v", path, got, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sameWakeFileIdentity(info, after) {
		t.Fatalf("preserved wake file %s identity changed", path)
	}
}
