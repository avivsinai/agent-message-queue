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
	inspection := inspectWakeLock(root, "codex")
	if inspection.Lock.StateGeneration != inspection.Lock.Generation ||
		inspection.Lock.StateDigest != inspection.Lock.TargetDigest ||
		inspection.Lock.StateDigest != state.State.Target.TargetDigest {
		t.Fatalf("authoritative bound lock = %#v, state target = %#v", inspection.Lock, state.State.Target)
	}
}

func TestAuthoritativeWakeAcquisitionStateFailureLeavesUnboundTargetShadow(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	injected := errors.New("state projection failed after owner commit")
	installWakeStatePublicationFailure(t, wakeStateAfterTempWrite, injected)

	_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("acquisition error = %v, want state failure", err)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Exists {
		t.Fatalf("pre-link state failure committed a wake lock: %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("target shadow = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	if _, stateExists := readOptionalWakeStateAtPathForTest(t, root, "codex"); stateExists {
		t.Fatal("state exists after pre-rename publication failure")
	}
}

func TestAuthoritativeWakeAcquisitionStateCrashSeamsLeaveUnboundShadows(t *testing.T) {
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

			_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target:   &target,
				wakeMode: wakeTargetInjectVia,
			})
			if !errors.Is(err, injected) {
				t.Fatalf("acquisition error = %v, want state failure", err)
			}
			if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
				t.Fatalf("pre-link state failure committed a wake lock: %#v", inspection)
			}
			persisted, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
				t.Fatalf("target shadow = %#v exists=%v err=%v", persisted, exists, readErr)
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

func TestBoundPreparedCurrentMarkerRefusesMalformedBindingWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*wakeLock)
	}{
		{
			name: "partial binding",
			mutate: func(lock *wakeLock) {
				lock.StateDigest = ""
			},
		},
		{
			name: "malformed binding",
			mutate: func(lock *wakeLock) {
				lock.StateGeneration = "not-a-valid-state-generation"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			stateRaw, stateInfo := snapshotWakeFileForTest(t, statePath)
			preparedRaw, preparedInfo := snapshotWakeFileForTest(t, fixture.preparedPath)

			lock := fixture.inspection.Lock
			test.mutate(&lock)
			replaceWakeLockForTest(t, fixture.lockPath, lock)
			fixture.inspection = inspectWakeLock(fixture.root, fixture.me)

			err := writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
			var bound *wakeStateBoundInconclusiveError
			if !errors.As(err, &bound) {
				t.Fatalf("prepared publication error = %v, want bound refusal", err)
			}
			assertWakeFileSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
			assertWakeFileSnapshotUnchangedForTest(t, fixture.preparedPath, preparedRaw, preparedInfo)
		})
	}
}

func TestBoundPreparedCurrentMarkerRefusesPostSelectionLockReplacement(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	stateRaw, stateInfo := installWakeStatePreparedMismatchForTest(t, statePath)
	preparedRaw, preparedInfo := snapshotWakeFileForTest(t, fixture.preparedPath)

	original := afterWakePreparedBoundValidation
	afterWakePreparedBoundValidation = func() {
		afterWakePreparedBoundValidation = func() {}
		replaceWakeLockForTest(t, fixture.lockPath, fixture.inspection.Lock)
	}
	t.Cleanup(func() { afterWakePreparedBoundValidation = original })

	err := writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
	var bound *wakeStateBoundInconclusiveError
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &bound) || !errors.As(err, &changed) {
		t.Fatalf("prepared publication error = %v, want bound replacement refusal", err)
	}
	assertWakeFileSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
	assertWakeFileSnapshotUnchangedForTest(t, fixture.preparedPath, preparedRaw, preparedInfo)
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

			if preparedErr := writeWakePreparedFile(root, "codex", inspection); !errors.Is(preparedErr, injected) {
				t.Fatalf("prepared publication error = %v, want state refresh failure", preparedErr)
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
	if fixture.created.Lock.StateGeneration != fixture.created.Lock.Generation ||
		fixture.created.Lock.StateDigest != fixture.created.Lock.TargetDigest ||
		fixture.created.Lock.StateDigest != state.State.Target.TargetDigest {
		t.Fatalf("generic bound lock = %#v, state target = %#v", fixture.created.Lock, state.State.Target)
	}
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

func TestGenericWakeStateCrashSeamsLeaveUnboundShadows(t *testing.T) {
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

			_, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target:   &target,
				wakeMode: wakeTargetInjectVia,
			})
			if !errors.Is(err, injected) {
				t.Fatalf("generic acquisition error = %v, want state failure", err)
			}
			inspection := inspectWakeLock(root, "codex")
			if inspection.Exists {
				t.Fatalf("pre-link state failure committed a generic wake lock: %#v", inspection)
			}
			persisted, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
				t.Fatalf("committed target = %#v exists=%v err=%v", persisted, exists, readErr)
			}
			_, stateExists := readOptionalWakeStateAtPathForTest(t, root, "codex")
			if stateExists != test.wantState {
				t.Fatalf("state exists=%v, want %v", stateExists, test.wantState)
			}
		})
	}
}

func TestGenericBoundLockSyncFailurePreservesCommittedClaim(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "generic-bound-sync-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)

	originalSync := syncWakeLockAfterCommitDirFD
	syncWakeLockAfterCommitDirFD = func(int) error {
		return syscall.EIO
	}
	t.Cleanup(func() { syncWakeLockAfterCommitDirFD = originalSync })

	_, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("generic acquisition error = %v, want post-link sync failure", err)
	}
	inspection := inspectWakeLock(root, "codex")
	if !inspection.Exists || inspection.Lock.StateGeneration != inspection.Lock.Generation ||
		inspection.Lock.StateDigest != inspection.Lock.TargetDigest {
		t.Fatalf("post-link generic claim = %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("post-link generic target = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	state := readWakeStateAtPathForTest(t, root, "codex")
	if state.State.Target.TargetDigest != inspection.Lock.StateDigest {
		t.Fatalf("post-link generic state target = %#v, lock = %#v", state.State.Target, inspection.Lock)
	}
}

func TestExistingGenericTargetLockIsNotUpgradedToStateBinding(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	legacy := fixture.created.Lock
	legacy.StateGeneration = ""
	legacy.StateDigest = ""
	legacy.PID = 4242
	legacy.ProcessStart = "legacy-start"
	legacy.BootID = "legacy-boot"
	legacy.Executable = "/opt/amq"
	legacy.ImagePath = ""
	legacy.ImageVersion = ""
	legacy.Args = []string{"amq", "wake", "--root", fixture.root, "--me", fixture.me}
	legacy.RunningImageEvidence = nil
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: legacy.ProcessStart,
			BootID:     legacy.BootID,
			Executable: legacy.Executable,
			Args:       legacy.Args,
		}
	})
	lockPath := writeWakeLockExactForTest(t, fixture.root, fixture.me, legacy)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if inspection := inspectWakeLock(fixture.root, fixture.me); inspection.Status != wakeLockValid {
		t.Fatalf("rewritten legacy fixture is not a live lock: %#v", inspection)
	}

	if _, err := acquireWakeLockWithOptions(fixture.root, fixture.me, fixture.options); err == nil {
		t.Fatal("existing target-bearing lock acquisition unexpectedly succeeded")
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("existing target-bearing lock was rewritten:\n got %s\nwant %s", after, before)
	}
	inspection := inspectWakeLock(fixture.root, fixture.me)
	if inspection.Lock.StateGeneration != "" || inspection.Lock.StateDigest != "" {
		t.Fatalf("existing target-bearing lock was upgraded: %#v", inspection.Lock)
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
	inspection := inspectWakeLock(fixture.root, fixture.me)
	if inspection.Lock.StateGeneration != "" || inspection.Lock.StateDigest != "" {
		t.Fatalf("targetless generic lock must remain unbound: %#v", inspection.Lock)
	}
	assertPathMissingForTest(t, statePath)
}

func TestRecoverOwnerPreservesOrphanTargetAndExactState(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)

	result, err := recoverOwnerWake(fixture.root, fixture.me)
	if err == nil || result.Status != "refused" {
		t.Fatalf("orphan recovery = %#v err=%v, want refused", result, err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
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

func TestUnsupportedOwnerPublicationPreservesTargetAndStateShadows(t *testing.T) {
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
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("target shadow = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	state := readWakeStateAtPathForTest(t, root, "codex")
	if !sameWakeTarget(state.State.Target.wakeTarget(), target) || state.State.Prepared != nil {
		t.Fatalf("state shadow = %#v", state.State)
	}
}

func TestOwnerPublicationStateReplacementBeforeLinkIsPreserved(t *testing.T) {
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
	linked := false
	publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
		linked = true
		return nil
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
	if acquireErr == nil || !strings.Contains(acquireErr.Error(), "wake state changed") {
		t.Fatalf("state replacement publication error=%v", acquireErr)
	}
	if linked {
		t.Fatal("state replacement reached the ownership link")
	}
	if strings.Contains(stderr, "continuing with legacy wake state") {
		t.Fatalf("state replacement warning = %q", stderr)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("target shadow = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	got, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Equal(got, state.Raw) {
		t.Fatalf("preserved replacement state bytes=%q err=%v", got, readErr)
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

func TestUnboundP2aAuthoritativeWakeReleaseRemovesInvalidStateAfterTarget(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
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

func TestUnboundP2aAuthoritativeWakeReleaseStateSnapshotFailureKeepsLegacyCleanupCommitted(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
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

func TestUnboundP2aPreparedMutationPreservesNewerWakeStateInstalledBeforeRename(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
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

func TestBoundPreparedMutationRejectsNewerWakeStateSchemas(t *testing.T) {
	for _, component := range []string{"document", "target", "prepared"} {
		t.Run(component, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			stateRaw, stateInfo := installNewerWakeStateSchemaForTest(t, statePath, component)

			writeErr := writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
			var bound *wakeStateBoundInconclusiveError
			if !errors.As(writeErr, &bound) {
				t.Fatalf("bound prepared mutation error = %v, want bound refusal", writeErr)
			}
			assertWakeStateSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
			marker, exists, err := readWakeGenerationFile(fixture.preparedPath, "wake prepared marker")
			if err != nil || !exists || marker.Generation != fixture.inspection.Lock.Generation {
				t.Fatalf("bound legacy prepared marker = %#v exists=%v err=%v", marker, exists, err)
			}
		})
	}
}

func TestBoundPreparedMutationRejectsNewerWakeStateInstalledBeforeRename(t *testing.T) {
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

	writeErr := writeWakePreparedFile(fixture.root, fixture.me, fixture.inspection)
	if writeErr == nil || !strings.Contains(writeErr.Error(), "refresh bound wake state") {
		t.Fatalf("bound prepared mutation error = %v, want state-refresh refusal", writeErr)
	}
	if !replaced {
		t.Fatal("newer state interleave did not run")
	}
	assertWakeStateSnapshotUnchangedForTest(t, statePath, newerRaw, installedInfo)
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

func TestUnboundP2aAuthoritativeReleasePreservesNewerWakeStateSchemas(t *testing.T) {
	for _, component := range []string{"document", "target", "prepared"} {
		t.Run(component, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
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
	before := snapshotWakeCheckTree(t, fixture.root)
	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeAuthoritativeWakeClaimAt(
			dirfd,
			fixture.agentDir,
			fixture.inspection,
			nil,
		)
	})
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("release error = %v, want bound inconclusive", err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state missing after refused release: %v", err)
	}
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
	assertWakeFileSnapshotUnchangedForTest(t, path, raw, info)
}

func snapshotWakeFileForTest(t *testing.T, path string) ([]byte, os.FileInfo) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw, info
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

func replaceWakeLockForTest(t *testing.T, lockPath string, lock wakeLock) {
	t.Helper()
	raw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := lockPath + ".replacement"
	if err := os.WriteFile(replacementPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacementPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, lockPath); err != nil {
		t.Fatal(err)
	}
}

func installWakeStatePreparedMismatchForTest(t *testing.T, statePath string) ([]byte, os.FileInfo) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeWakeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	state.Prepared = nil
	raw, err = encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return raw, info
}
