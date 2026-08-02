//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
}

func TestAuthoritativeWakeAcquisitionStateFailurePreservesCommittedLegacy(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	injected := errors.New("state projection failed after owner commit")
	installWakeStatePublicationFailure(t, wakeStateAfterTempWrite, injected)

	var cleanup func()
	var err error
	stderr := captureWakeStderr(t, func() {
		cleanup, err = acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &target,
			wakeMode: wakeTargetInjectVia,
		})
	})
	if err != nil || cleanup == nil {
		t.Fatalf("acquisition cleanup=%v err=%v, want success", cleanup != nil, err)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("projection warning = %q", stderr)
	}
	inspection := inspectWakeLock(root, "codex")
	if classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
		t.Fatalf("legacy lock was not committed: %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("legacy target = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	afterWakeStatePublicationBoundary = func(wakeStatePublicationBoundary) error { return nil }
	if err := writeWakePreparedFile(root, "codex", inspection); err != nil {
		t.Fatalf("next guarded mutation did not heal state: %v", err)
	}
	state := readWakeStateAtPathForTest(t, root, "codex")
	if state.State.Prepared == nil || state.State.Prepared.Generation != inspection.Lock.Generation {
		t.Fatalf("healed state prepared = %#v", state.State.Prepared)
	}
}

func TestAuthoritativeWakeAcquisitionStateCrashSeamsKeepCommittedLegacy(t *testing.T) {
	for _, test := range []struct {
		boundary  wakeStatePublicationBoundary
		wantState bool
	}{
		{boundary: wakeStateAfterTempWrite},
		{boundary: wakeStateAfterFileSync},
		{boundary: wakeStateAfterPreRenameDirSync},
		{boundary: wakeStateAfterRename, wantState: true},
		{boundary: wakeStateAfterPostRenameDirSync, wantState: true},
		{boundary: wakeStateAfterVerify, wantState: true},
	} {
		t.Run(string(test.boundary), func(t *testing.T) {
			root, target, _ := newOwnerAcquisitionPublicationFixture(t)
			injected := errors.New("owner state crash seam")
			installWakeStatePublicationFailure(t, test.boundary, injected)

			var cleanup func()
			var err error
			stderr := captureWakeStderr(t, func() {
				cleanup, err = acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
					target:   &target,
					wakeMode: wakeTargetInjectVia,
				})
			})
			if err != nil || cleanup == nil {
				t.Fatalf("acquisition cleanup=%v err=%v, want success", cleanup != nil, err)
			}
			if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
				t.Fatalf("projection warning = %q", stderr)
			}
			if inspection := inspectWakeLock(root, "codex"); classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
				t.Fatalf("legacy lock was not committed: %#v", inspection)
			}
			_, stateExists := readOptionalWakeStateAtPathForTest(t, root, "codex")
			if stateExists != test.wantState {
				t.Fatalf("state exists=%v, want %v", stateExists, test.wantState)
			}
		})
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
	var preparedErr error
	stderr := captureWakeStderr(t, func() {
		preparedErr = writeWakePreparedFile(root, "codex", inspection)
	})
	if preparedErr != nil {
		t.Fatalf("prepared publication error = %v, want success", preparedErr)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("projection warning = %q", stderr)
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

func TestWakePreparedStateCrashSeamsLeaveLegacyPreparedAuthoritative(t *testing.T) {
	for _, test := range []struct {
		boundary            wakeStatePublicationBoundary
		wantPreparedInState bool
	}{
		{boundary: wakeStateAfterTempWrite},
		{boundary: wakeStateAfterFileSync},
		{boundary: wakeStateAfterPreRenameDirSync},
		{boundary: wakeStateAfterRename, wantPreparedInState: true},
		{boundary: wakeStateAfterPostRenameDirSync, wantPreparedInState: true},
		{boundary: wakeStateAfterVerify, wantPreparedInState: true},
	} {
		t.Run(string(test.boundary), func(t *testing.T) {
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
			injected := errors.New("prepared state crash seam")
			installWakeStatePublicationFailure(t, test.boundary, injected)

			var preparedErr error
			stderr := captureWakeStderr(t, func() {
				preparedErr = writeWakePreparedFile(root, "codex", inspection)
			})
			if preparedErr != nil {
				t.Fatalf("prepared publication error = %v, want success", preparedErr)
			}
			if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
				t.Fatalf("projection warning = %q", stderr)
			}
			if _, exists, err := readWakeGenerationFile(wakePreparedPath(root, "codex"), "wake prepared marker"); err != nil || !exists {
				t.Fatalf("legacy prepared marker exists=%v err=%v", exists, err)
			}
			state := readWakeStateAtPathForTest(t, root, "codex")
			gotPrepared := state.State.Prepared != nil
			if gotPrepared != test.wantPreparedInState {
				t.Fatalf("state prepared exists=%v, want %v", gotPrepared, test.wantPreparedInState)
			}
		})
	}
}

func TestGenericWakeTargetAndPreparedMutationsRefreshState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	state := readWakeStateAtPathForTest(t, fixture.root, fixture.me)
	if state.State.Prepared == nil || state.State.Prepared.Generation != fixture.created.Lock.Generation {
		t.Fatalf("generic prepared state = %#v", state.State.Prepared)
	}

	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	state = readWakeStateAtPathForTest(t, fixture.root, fixture.me)
	if state.State.Prepared != nil {
		t.Fatalf("generic cleanup state prepared = %#v, want nil", state.State.Prepared)
	}
}

func TestGenericWakeCleanupProjectionFailureKeepsLegacyCleanupCommitted(t *testing.T) {
	for _, boundary := range []wakeStatePublicationBoundary{
		wakeStateAfterTempWrite,
		wakeStateAfterFileSync,
		wakeStateAfterPreRenameDirSync,
		wakeStateAfterRename,
		wakeStateAfterPostRenameDirSync,
		wakeStateAfterVerify,
	} {
		t.Run(string(boundary), func(t *testing.T) {
			fixture := newGenericWakePreparedCleanupFixture(t, true)
			injected := errors.New("state projection failed after generic cleanup")
			installWakeStatePublicationFailure(t, boundary, injected)

			var cleanupErr error
			stderr := captureWakeStderr(t, func() {
				cleanupErr = fixture.cleanupNow()
			})
			if cleanupErr != nil {
				t.Fatalf("generic cleanup error = %v, want success", cleanupErr)
			}
			if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
				t.Fatalf("generic cleanup projection warning = %q", stderr)
			}
			if inspection := inspectWakeLock(fixture.root, fixture.me); inspection.Exists {
				t.Fatalf("generic lock survived committed cleanup: %#v", inspection)
			}
			assertPathMissingForTest(t, fixture.preparedPath)
			if _, exists, err := readWakeTarget(fixture.root, fixture.me); err != nil || !exists {
				t.Fatalf("generic target exists=%v err=%v after cleanup", exists, err)
			}

			afterWakeStatePublicationBoundary = func(wakeStatePublicationBoundary) error { return nil }
			cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, fixture.options)
			if err != nil {
				t.Fatalf("next guarded acquisition did not heal state: %v", err)
			}
			t.Cleanup(cleanup)
			state := readWakeStateAtPathForTest(t, fixture.root, fixture.me)
			if state.State.Prepared != nil {
				t.Fatalf("healed generic cleanup state prepared = %#v, want nil", state.State.Prepared)
			}
		})
	}
}

func TestGenericWakeStateCrashSeamsKeepCommittedLegacy(t *testing.T) {
	for _, test := range []struct {
		boundary  wakeStatePublicationBoundary
		wantState bool
	}{
		{boundary: wakeStateAfterTempWrite},
		{boundary: wakeStateAfterFileSync},
		{boundary: wakeStateAfterPreRenameDirSync},
		{boundary: wakeStateAfterRename, wantState: true},
		{boundary: wakeStateAfterPostRenameDirSync, wantState: true},
		{boundary: wakeStateAfterVerify, wantState: true},
	} {
		t.Run(string(test.boundary), func(t *testing.T) {
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "generic-state-gap-injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			injected := errors.New("state projection failed after generic lock commit")
			installWakeStatePublicationFailure(t, test.boundary, injected)

			var cleanup func()
			var err error
			stderr := captureWakeStderr(t, func() {
				cleanup, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
					target:   &target,
					wakeMode: wakeTargetInjectVia,
				})
			})
			if err != nil || cleanup == nil {
				t.Fatalf("generic acquisition cleanup=%v err=%v, want success", cleanup != nil, err)
			}
			if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, injected.Error()) {
				t.Fatalf("projection warning = %q", stderr)
			}
			inspection := inspectWakeLock(root, "codex")
			if !inspection.Exists {
				t.Fatal("projection failure did not preserve committed generic lock")
			}
			persisted, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
				t.Fatalf("committed target = %#v exists=%v err=%v", persisted, exists, readErr)
			}
			_, stateExists := readOptionalWakeStateAtPathForTest(t, root, "codex")
			if stateExists != test.wantState {
				t.Fatalf("state exists=%v, want %v", stateExists, test.wantState)
			}
			afterWakeStatePublicationBoundary = func(wakeStatePublicationBoundary) error { return nil }
			if err := writeWakePreparedFile(root, "codex", inspection); err != nil {
				t.Fatalf("next guarded mutation did not heal state: %v", err)
			}
			state := readWakeStateAtPathForTest(t, root, "codex")
			if state.State.Prepared == nil || state.State.Prepared.Generation != inspection.Lock.Generation {
				t.Fatalf("healed generic state prepared = %#v", state.State.Prepared)
			}
		})
	}
}

func TestGenericWakeWithoutTargetRemovesStaleState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("stale state missing before targetless acquisition: %v", err)
	}

	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{
		wakeMode: wakeInjectModeNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	assertPathMissingForTest(t, statePath)
}

func TestRecoverOwnerRemovesOrphanTargetAndExactState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state missing before orphan recovery: %v", err)
	}

	result, err := recoverOwnerWake(fixture.root, fixture.me)
	if err != nil || result.Status != "recovered" {
		t.Fatalf("orphan recovery = %#v err=%v", result, err)
	}
	assertPathMissingForTest(t, wakeTargetPath(fixture.root, fixture.me))
	assertPathMissingForTest(t, statePath)
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

func TestRecoverOwnerStateRemovalFailureKeepsTargetlessRecoveryCommitted(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := statePath + ".replacement"
	if err := os.WriteFile(replacementPath, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := afterWakeStateSnapshotRead
	reads := 0
	afterWakeStateSnapshotRead = func() {
		reads++
		if reads == 2 {
			if err := os.Rename(replacementPath, statePath); err != nil {
				t.Fatalf("install recovery state replacement: %v", err)
			}
		}
	}
	t.Cleanup(func() { afterWakeStateSnapshotRead = originalHook })

	var result wakeOwnerRecoverResult
	stderr := captureWakeStderr(t, func() {
		result, err = recoverOwnerWake(fixture.root, fixture.me)
	})
	if err != nil || result.Status != "recovered" {
		t.Fatalf("targetless recovery = %#v err=%v", result, err)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, "wake state changed") {
		t.Fatalf("targetless recovery projection warning = %q", stderr)
	}
	got, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Equal(got, stateRaw) {
		t.Fatalf("preserved recovery state bytes=%q err=%v", got, readErr)
	}
}

func TestUnsupportedOwnerPublicationRemovesStateAfterExactTargetCleanup(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}
	if _, err := publishWakeStateForTest(fixture, captureWakeStateLegacyForTest(t, fixture)); err != nil {
		t.Fatal(err)
	}

	originalLink := publishAuthoritativeWakeLinkAt
	publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
		return syscall.EOPNOTSUPP
	}
	t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })

	if _, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	}); !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("unsupported publication error=%v", err)
	}
	assertPathMissingForTest(t, wakeTargetPath(root, "codex"))
	assertPathMissingForTest(t, filepath.Join(agentDir.path, wakeStateFileName))
}

func TestUnsupportedOwnerPublicationStateRemovalFailureKeepsTargetCleanupCommitted(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}
	state, err := publishWakeStateForTest(fixture, captureWakeStateLegacyForTest(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(agentDir.path, wakeStateFileName)
	replacementPath := statePath + ".replacement"
	if err := os.WriteFile(replacementPath, state.Raw, 0o600); err != nil {
		t.Fatal(err)
	}

	originalLink := publishAuthoritativeWakeLinkAt
	publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
		return syscall.EOPNOTSUPP
	}
	t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })
	originalReadHook := afterWakeStateSnapshotRead
	reads := 0
	afterWakeStateSnapshotRead = func() {
		reads++
		if reads == 2 {
			if err := os.Rename(replacementPath, statePath); err != nil {
				t.Fatalf("install unsupported-cleanup state replacement: %v", err)
			}
		}
	}
	t.Cleanup(func() { afterWakeStateSnapshotRead = originalReadHook })

	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		_, acquireErr = acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &target,
			wakeMode: wakeTargetInjectVia,
		})
	})
	if !errors.Is(acquireErr, syscall.EOPNOTSUPP) {
		t.Fatalf("unsupported publication error=%v", acquireErr)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, "wake state changed") {
		t.Fatalf("unsupported cleanup projection warning = %q", stderr)
	}
	assertPathMissingForTest(t, wakeTargetPath(root, "codex"))
	got, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Equal(got, state.Raw) {
		t.Fatalf("preserved unsupported state bytes=%q err=%v", got, readErr)
	}
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

func TestAuthoritativeWakeReleaseRemovesInvalidStateAfterTarget(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if err := os.WriteFile(statePath, []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, statePath)
}

func TestAuthoritativeWakeReleaseStateSnapshotFailureKeepsLegacyCleanupCommitted(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statePath, 0o644); err != nil {
		t.Fatal(err)
	}

	var releaseErr error
	stderr := captureWakeStderr(t, func() {
		releaseErr = fixture.release()
	})
	if releaseErr != nil {
		t.Fatalf("release error = %v, want successful legacy release", releaseErr)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") ||
		!strings.Contains(stderr, "must be a regular 0600 file") {
		t.Fatalf("state snapshot warning = %q", stderr)
	}
	fixture.assertReleasedClaimMissing(t)
	got, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Equal(got, stateRaw) {
		t.Fatalf("preserved unreadable state bytes=%q err=%v", got, readErr)
	}
}

func TestGuardedPreparedMutationPreservesNewerWakeStateSchemas(t *testing.T) {
	for _, component := range []string{"document", "target", "prepared"} {
		t.Run(component, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
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

func TestGuardedPreparedMutationPreservesNewerWakeStateInstalledBeforeRename(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	currentRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	newerRaw := newerWakeStateRawForTest(t, currentRaw, "document")
	replacementPath := statePath + ".newer"
	if err := os.WriteFile(replacementPath, newerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	originalBoundary := afterWakeStatePublicationBoundary
	replaced := false
	var installedInfo os.FileInfo
	afterWakeStatePublicationBoundary = func(boundary wakeStatePublicationBoundary) error {
		if boundary == wakeStateAfterPreRenameDirSync && !replaced {
			replaced = true
			if err := os.Rename(replacementPath, statePath); err != nil {
				return err
			}
			var err error
			installedInfo, err = os.Stat(statePath)
			return err
		}
		return nil
	}
	t.Cleanup(func() { afterWakeStatePublicationBoundary = originalBoundary })

	var writeErr error
	stderr := captureWakeStderr(t, func() {
		writeErr = writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
	})
	if writeErr != nil {
		t.Fatalf("prepared mutation error = %v, want successful legacy commit", writeErr)
	}
	if !replaced {
		t.Fatal("newer state interleave did not run")
	}
	assertSingleWakeStateProjectionWarning(t, stderr)
	assertWakeStateSnapshotUnchangedForTest(t, statePath, newerRaw, installedInfo)
}

func TestAuthoritativeReleasePreservesNewerWakeStateSchemas(t *testing.T) {
	for _, component := range []string{"document", "target", "prepared"} {
		t.Run(component, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			stateRaw, stateInfo := installNewerWakeStateSchemaForTest(t, statePath, component)

			var releaseErr error
			stderr := captureWakeStderr(t, func() {
				releaseErr = fixture.release()
			})
			if releaseErr != nil {
				t.Fatalf("release error = %v, want successful legacy release", releaseErr)
			}
			assertSingleWakeStateProjectionWarning(t, stderr)
			fixture.assertReleasedClaimMissing(t)
			assertPathMissingForTest(t, fixture.preparedPath)
			assertWakeStateSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
		})
	}
}

func TestAuthoritativeWakeReleaseRemovesStateWhenTargetAlreadyMissing(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	if err := os.Remove(fixture.preparedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.targetPath); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeAuthoritativeWakeClaimAt(
			dirfd,
			fixture.agentDir,
			fixture.inspection,
			nil,
		)
	}); err != nil {
		t.Fatal(err)
	}
	assertPathMissingForTest(t, fixture.lockPath)
	assertPathMissingForTest(t, statePath)
}

func TestAuthoritativeWakeReleasePreservesStateReplacement(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	originalRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := statePath + ".replacement"
	if err := os.WriteFile(replacementPath, originalRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	originalHook := removeAuthoritativeWakeAfterLockRelease
	removeAuthoritativeWakeAfterLockRelease = func() {
		if err := os.Rename(replacementPath, statePath); err != nil {
			t.Errorf("install state replacement: %v", err)
		}
	}
	t.Cleanup(func() { removeAuthoritativeWakeAfterLockRelease = originalHook })

	var releaseErr error
	stderr := captureWakeStderr(t, func() {
		releaseErr = fixture.release()
	})
	if releaseErr != nil {
		t.Fatalf("release error = %v, want successful legacy release", releaseErr)
	}
	if !strings.Contains(stderr, "continuing with legacy wake state") || !strings.Contains(stderr, "preserving") {
		t.Fatalf("replacement preservation warning = %q", stderr)
	}
	fixture.assertReleasedClaimMissing(t)
	got, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Equal(got, originalRaw) {
		t.Fatalf("replacement state bytes=%q err=%v", got, readErr)
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
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("preserved wake state bytes=%q err=%v", got, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sameWakeFileIdentity(info, after) {
		t.Fatal("preserved wake state identity changed")
	}
}
