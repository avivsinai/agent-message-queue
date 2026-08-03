//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type authoritativeWakePreparedCleanupFixture struct {
	root           string
	me             string
	agentDir       *wakeAgentDir
	target         wakeTarget
	inspection     wakeLockInspection
	lockPath       string
	targetPath     string
	preparedPath   string
	preparedMarker wakeReady
	preparedRaw    []byte
}

func TestAuthoritativeWakeCleanupRemovesExactPreparedMarker(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)

	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}

	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeAcquireDetachedDuringReleaseStopsWithoutReacquiring(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	names := []string{".wake.lock", wakeTargetFileName, wakeStateFileName, wakePreparedFileName}
	claimBefore := snapshotDetachedWakeFiles(t, fixture.agentDir.path, names...)
	detachedPath := fixture.agentDir.path + ".detached"
	var successorBefore map[string]detachedWakeFileSnapshot
	installAuthoritativeWakeFinalAuthorityInterleave(t, func() {
		if err := os.Rename(fixture.agentDir.path, detachedPath); err != nil {
			t.Fatalf("detach authoritative wake agent directory: %v", err)
		}
		if err := os.Mkdir(fixture.agentDir.path, 0o700); err != nil {
			t.Fatalf("create authoritative successor wake agent directory: %v", err)
		}
		for name, snapshot := range claimBefore {
			if err := os.WriteFile(
				filepath.Join(fixture.agentDir.path, name),
				snapshot.raw,
				snapshot.info.Mode().Perm(),
			); err != nil {
				t.Fatalf("write authoritative successor %s: %v", name, err)
			}
		}
		successorBefore = snapshotDetachedWakeFiles(t, fixture.agentDir.path, names...)
	})

	requestedOwner := *fixture.target.Owner
	requestedOwner.SessionID++
	requested := fixture.target
	requested.Owner = &requestedOwner
	originalObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(owner wakeOwner) (wakeOwnerObservation, error) {
		if sameWakeOwner(&owner, fixture.target.Owner) {
			return deadWakeOwnerObservation("old owner is dead"), nil
		}
		if sameWakeOwner(&owner, &requestedOwner) {
			return wakeOwnerObservation{State: wakeOwnerSame}, nil
		}
		return wakeOwnerObservation{State: wakeOwnerUnknown, Reason: "unexpected owner"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireAuthoritativeWakeLockWithOptionsInDir(
		fixture.agentDir,
		fixture.root,
		fixture.me,
		wakeLockAcquireOptions{target: &requested, wakeMode: wakeTargetInjectVia},
	)
	if cleanup != nil {
		t.Fatal("detached authoritative acquisition returned cleanup authority")
	}
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("authoritative detached release error = %v, want wakeDetachedCleanupOnlyError", err)
	}
	if _, statErr := os.Stat(filepath.Join(detachedPath, ".wake.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("detached authoritative lock = %v, want absent", statErr)
	}
	assertDetachedWakeFilesUnchanged(t, fixture.agentDir.path, successorBefore)
}

func TestAuthoritativeWakeCleanupDeadOwnerExactPreparedAllowsFirstReacquire(t *testing.T) {
	root := secureTempDirForTest(t)
	const me = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatal(err)
	}
	oldOwner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	injector := writeExecutableForTest(t, "dead-owner-prepared-cleanup-injector")
	oldTarget := mustNewWakeTargetForTest(t, root, me, injector, nil)
	oldTarget.Owner = &oldOwner
	oldLock, err := newWakeLock(root, me, wakeLockAcquireOptions{
		target:   &oldTarget,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		if err := publishAuthoritativeWakeClaimAt(
			dirfd,
			agentDir,
			root,
			me,
			oldTarget,
			oldLock,
		); err != nil {
			return err
		}
		if err := writeWakeGenerationFileAt(
			dirfd,
			wakePreparedFileName,
			"wake prepared marker",
			wakeReady{
				Schema:       wakeReadySchema,
				Generation:   oldLock.Generation,
				TargetDigest: oldLock.TargetDigest,
			},
		); err != nil {
			return err
		}
		return reconcileWakeStateAfterLegacyMutationAt(dirfd, agentDir, root, me)
	}); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(root, me)
	if classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
		t.Fatalf("old owner wake claim = %#v, want authoritative", inspection)
	}
	originalObserve := observeAuthoritativeWakeOwner
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })
	observeCalls := 0
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		observeCalls++
		return deadWakeOwnerObservation("old owner is dead"), nil
	}
	releaseErr := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return removeAuthoritativeWakeClaimAt(
			dirfd,
			agentDir,
			inspection,
			&oldTarget,
		)
	})
	observeAuthoritativeWakeOwner = originalObserve
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if observeCalls != 0 {
		t.Fatalf("authoritative release observed old owner liveness %d times", observeCalls)
	}
	assertPathMissingForTest(t, wakePreparedPath(root, me))

	newOwner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	newTarget := mustNewWakeTargetForTest(t, root, me, injector, nil)
	newTarget.Owner = &newOwner
	cleanup, err := acquireAuthoritativeWakeLockWithOptions(
		root,
		me,
		wakeLockAcquireOptions{
			target:   &newTarget,
			wakeMode: wakeTargetInjectVia,
		},
	)
	if err != nil {
		t.Fatalf("first reacquire after dead-owner release: %v", err)
	}
	if cleanup == nil {
		t.Fatal("first reacquire returned no cleanup authority")
	}
	reacquired := inspectWakeLock(root, me)
	if classifyPersistedWakeClaim(reacquired) != wakeClaimAuthoritative ||
		!sameWakeOwner(reacquired.Lock.Owner, &newOwner) {
		t.Fatalf("first reacquired wake claim = %#v", reacquired)
	}
	cleanup()
}

func TestAuthoritativeWakeCleanupPreservesWrongPreparedMarker(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*wakeReady)
	}{
		{
			name: "generation",
			change: func(marker *wakeReady) {
				marker.Generation = "wrong-generation"
			},
		},
		{
			name: "target digest",
			change: func(marker *wakeReady) {
				marker.TargetDigest = "sha256:" + strings.Repeat("f", 64)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			replacement := fixture.preparedMarker
			test.change(&replacement)
			replacementRaw := writeAuthoritativePreparedMarkerForTest(
				t,
				fixture.preparedPath,
				replacement,
			)

			before := snapshotWakeCheckTree(t, fixture.root)
			err := fixture.release()
			var inconclusive *wakeStateBoundInconclusiveError
			if !errors.As(err, &inconclusive) {
				t.Fatalf("release error = %v, want bound inconclusive", err)
			}
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
			assertFileRawForTest(t, fixture.preparedPath, replacementRaw)
		})
	}
}

func TestAuthoritativeWakeCleanupPreservesChangedPreparedMarker(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	replacementRaw := append([]byte(" \n"), fixture.preparedRaw...)
	installAuthoritativeWakeCleanupInterleave(t, func() {
		if err := os.WriteFile(fixture.preparedPath, replacementRaw, 0o600); err != nil {
			t.Fatal(err)
		}
	})

	err := fixture.release()
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("changed prepared cleanup error = %v, want preservation", err)
	}
	fixture.assertReleasedClaimMissing(t)
	assertFileRawForTest(t, fixture.preparedPath, replacementRaw)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeCleanupPreservesNewInodePreparedReplacement(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	replacementPath := fixture.preparedPath + ".replacement"
	if err := os.WriteFile(replacementPath, fixture.preparedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(fixture.preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	installAuthoritativeWakeCleanupInterleave(t, func() {
		if err := os.Rename(replacementPath, fixture.preparedPath); err != nil {
			t.Fatal(err)
		}
	})

	err = fixture.release()
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("new-inode prepared cleanup error = %v, want preservation", err)
	}
	after, statErr := os.Stat(fixture.preparedPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if os.SameFile(before, after) {
		t.Fatal("prepared replacement unexpectedly retained the released inode")
	}
	fixture.assertReleasedClaimMissing(t)
	assertFileRawForTest(t, fixture.preparedPath, fixture.preparedRaw)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeCleanupPreservesSameInodePreparedReplacement(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	replacement := fixture.preparedMarker
	replacement.Generation = "same-inode-replacement"
	replacementRaw, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	replacementRaw = append(replacementRaw, '\n')
	before, err := os.Stat(fixture.preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	installAuthoritativeWakeCleanupInterleave(t, func() {
		if err := os.WriteFile(fixture.preparedPath, replacementRaw, 0o600); err != nil {
			t.Fatal(err)
		}
	})

	err = fixture.release()
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("same-inode prepared cleanup error = %v, want preservation", err)
	}
	after, statErr := os.Stat(fixture.preparedPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !os.SameFile(before, after) {
		t.Fatal("same-inode prepared mutation unexpectedly replaced its inode")
	}
	fixture.assertReleasedClaimMissing(t)
	assertFileRawForTest(t, fixture.preparedPath, replacementRaw)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeCleanupReplacementLockPreservesPreparedAndTarget(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	replacement := fixture.inspection.Lock
	replacement.Generation = "replacement-lock-generation"
	replacementRaw, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	replacementRaw = append(replacementRaw, '\n')
	installAuthoritativeWakeCleanupInterleave(t, func() {
		if err := os.WriteFile(fixture.lockPath, replacementRaw, wakeOwnerLockFileMode); err != nil {
			t.Fatal(err)
		}
	})

	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}

	assertFileRawForTest(t, fixture.lockPath, replacementRaw)
	assertFileRawForTest(t, fixture.preparedPath, fixture.preparedRaw)
	if _, err := os.Stat(fixture.targetPath); err != nil {
		t.Fatalf("replacement lock did not preserve wake target: %v", err)
	}
	fixture.assertControlSocketPresent(t)
}

func TestAuthoritativeWakeCleanupInvalidPreparedRefusesBoundMutation(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	invalid := []byte("{not-json\n")
	if err := os.WriteFile(fixture.preparedPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}

	before := snapshotWakeCheckTree(t, fixture.root)
	err := fixture.release()
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("invalid prepared cleanup error = %v, want bound inconclusive", err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
	assertFileRawForTest(t, fixture.preparedPath, invalid)
}

func TestUnboundP2aAuthoritativeWakeCleanupInvalidPreparedStillCleansExactClaim(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	unbindAuthoritativeWakePreparedFixtureForP2a(t, fixture)
	invalid := []byte("{not-json\n")
	if err := os.WriteFile(fixture.preparedPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}

	err := fixture.release()
	if err == nil || !strings.Contains(err.Error(), "snapshot released wake prepared marker") {
		t.Fatalf("invalid prepared cleanup error = %v, want snapshot error", err)
	}
	fixture.assertReleasedClaimMissing(t)
	assertFileRawForTest(t, fixture.preparedPath, invalid)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeCleanupPreparedSyncFailureContinuesCleanup(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	originalSync := syncWakeOwnerDirFD
	syncCalls := 0
	syncWakeOwnerDirFD = func(int) error {
		syncCalls++
		if syncCalls == 3 {
			return syscall.ENOSYS
		}
		return nil
	}
	t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

	err := fixture.release()
	if err == nil || !strings.Contains(err.Error(), "sync wake prepared marker removal") {
		t.Fatalf("prepared sync cleanup error = %v, want durability error", err)
	}
	if syncCalls != 4 {
		t.Fatalf("directory sync calls = %d, want 4", syncCalls)
	}
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
	fixture.assertControlSocketMissing(t)
}

func TestAuthoritativeWakeCleanupFinalSyncFailureIsReported(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	originalSync := syncWakeOwnerDirFD
	syncCalls := 0
	syncWakeOwnerDirFD = func(int) error {
		syncCalls++
		if syncCalls == 4 {
			return syscall.ENOSYS
		}
		return nil
	}
	t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

	err := fixture.release()
	if err == nil || !strings.Contains(err.Error(), "sync authoritative wake claim cleanup") {
		t.Fatalf("final sync cleanup error = %v, want durability error", err)
	}
	if syncCalls != 4 {
		t.Fatalf("directory sync calls = %d, want 4", syncCalls)
	}
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
	fixture.assertControlSocketMissing(t)
}

func newAuthoritativeWakePreparedCleanupFixture(
	t *testing.T,
) *authoritativeWakePreparedCleanupFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	const me = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatal(err)
	}
	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "authoritative-prepared-cleanup-injector")
	target := mustNewWakeTargetForTest(t, root, me, injector, nil)
	target.Owner = &owner
	lock, err := newWakeLock(root, me, wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, me, target, lock)
	}); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(root, me)
	if classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
		t.Fatalf("published wake claim = %#v, want authoritative", inspection)
	}
	if err := writeWakePreparedFile(root, me, inspection); err != nil {
		t.Fatal(err)
	}
	preparedPath := wakePreparedPath(root, me)
	preparedRaw, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker wakeReady
	if err := json.Unmarshal(preparedRaw, &marker); err != nil {
		t.Fatal(err)
	}
	if lock.ControlSocket != "" {
		if err := os.WriteFile(lock.ControlSocket, []byte("socket"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &authoritativeWakePreparedCleanupFixture{
		root:           root,
		me:             me,
		agentDir:       agentDir,
		target:         target,
		inspection:     inspection,
		lockPath:       filepath.Join(fsq.AgentBase(root, me), ".wake.lock"),
		targetPath:     wakeTargetPath(root, me),
		preparedPath:   preparedPath,
		preparedMarker: marker,
		preparedRaw:    preparedRaw,
	}
}

func (fixture *authoritativeWakePreparedCleanupFixture) release() error {
	return withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeAuthoritativeWakeClaimAt(
			dirfd,
			fixture.agentDir,
			fixture.inspection,
			&fixture.target,
		)
	})
}

func (fixture *authoritativeWakePreparedCleanupFixture) assertReleasedClaimMissing(t *testing.T) {
	t.Helper()
	assertPathMissingForTest(t, fixture.lockPath)
	assertPathMissingForTest(t, fixture.targetPath)
}

func (fixture *authoritativeWakePreparedCleanupFixture) assertControlSocketMissing(t *testing.T) {
	t.Helper()
	if path := fixture.inspection.Lock.ControlSocket; path != "" {
		assertPathMissingForTest(t, path)
	}
}

func (fixture *authoritativeWakePreparedCleanupFixture) assertControlSocketPresent(t *testing.T) {
	t.Helper()
	if path := fixture.inspection.Lock.ControlSocket; path != "" {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("replacement lock did not preserve control socket: %v", err)
		}
	}
}

func installAuthoritativeWakeCleanupInterleave(t *testing.T, fn func()) {
	t.Helper()
	original := removeAuthoritativeWakeAfterLockRelease
	removeAuthoritativeWakeAfterLockRelease = fn
	t.Cleanup(func() { removeAuthoritativeWakeAfterLockRelease = original })
}

func installAuthoritativeWakeFinalAuthorityInterleave(t *testing.T, fn func()) {
	t.Helper()
	original := removeAuthoritativeWakeBeforeFinalAuthorityCheck
	removeAuthoritativeWakeBeforeFinalAuthorityCheck = fn
	t.Cleanup(func() { removeAuthoritativeWakeBeforeFinalAuthorityCheck = original })
}

func writeAuthoritativePreparedMarkerForTest(
	t *testing.T,
	path string,
	marker wakeReady,
) []byte {
	t.Helper()
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPathMissingForTest(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s stat error = %v, want not exist", path, err)
	}
}
