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
)

func TestCrashContractTargetPublicationFailureBeforeLockPreservesInstalledTarget(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	originalLink := publishAuthoritativeWakeLinkAt
	publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
		return syscall.EIO
	}
	t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })

	_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("target-before-lock publication error = %v, want EIO", err)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("target-before-lock failure left lock: %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("target-before-lock failure target=%#v exists=%v err=%v", persisted, exists, readErr)
	}
}

func TestCrashContractLockReplacementIsRejectedByEveryFDReader(t *testing.T) {
	readers := []struct {
		name string
		read func(int, *wakeAgentDir, string, string) wakeLockInspection
	}{
		{name: "inspection", read: inspectWakeLockAt},
		{name: "metadata", read: readWakeLockMetadataAt},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			lock := wakeLock{PID: 4242, Generation: "reader-generation"}
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			agentDir, err := openWakeAgentDir(root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agentDir.Close() }()

			originalAfterRead := afterWakeLockAtRead
			afterWakeLockAtRead = func() {
				afterWakeLockAtRead = func() {}
				data, marshalErr := json.Marshal(lock)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				replacement := lockPath + ".replacement"
				if writeErr := os.WriteFile(replacement, data, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
				if renameErr := os.Rename(replacement, lockPath); renameErr != nil {
					t.Fatal(renameErr)
				}
			}
			t.Cleanup(func() { afterWakeLockAtRead = originalAfterRead })

			var inspection wakeLockInspection
			if err := agentDir.withFD(func(dirfd int) error {
				inspection = reader.read(dirfd, agentDir, root, "codex")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var changed *wakeSnapshotReadChangedError
			if inspection.Status != wakeLockUnverified ||
				!errors.As(inspection.observationErr, &changed) {
				t.Fatalf("replacement reader result = %#v, want typed unverified snapshot", inspection)
			}
		})
	}
}

func TestCrashContractPreparedMarkerDistinguishesStaleAndCurrentGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := os.MkdirAll(filepath.Dir(wakePreparedPath(root, "codex")), 0o700); err != nil {
		t.Fatal(err)
	}
	current := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Root:              canonicalWakeRoot(root),
		Agent:             "codex",
		Lock: wakeLock{
			Root:       canonicalWakeRoot(root),
			Agent:      "codex",
			Generation: "current-generation",
		},
	}
	for _, test := range []struct {
		name       string
		generation string
		wantReady  bool
	}{
		{name: "stale", generation: "stale-generation"},
		{name: "current", generation: current.Lock.Generation, wantReady: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := writeWakeGenerationFile(
				wakePreparedPath(root, "codex"),
				"wake prepared marker",
				wakeReady{Schema: wakeReadySchema, Generation: test.generation},
			); err != nil {
				t.Fatal(err)
			}
			ready, err := validateWakePreparedFileAgainstInspection(root, "codex", current)
			if err != nil || ready != test.wantReady {
				t.Fatalf("prepared generation %q ready=%v err=%v, want %v", test.generation, ready, err, test.wantReady)
			}
		})
	}
}

func TestCrashContractReadinessCleanupPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wake.ready")
	publication, err := publishWakeReadyFile(path, wakeReady{
		Schema: wakeReadySchema, Generation: "original-generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publication.Close() }()
	replacement := wakeReady{Schema: wakeReadySchema, Generation: "replacement-generation"}
	originalBeforeCleanup := beforeWakeReadyCleanupUnlink
	beforeWakeReadyCleanupUnlink = func() {
		beforeWakeReadyCleanupUnlink = func() {}
		if err := writeWakeGenerationFile(path, "replacement wake ready file", replacement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWakeReadyCleanupUnlink = originalBeforeCleanup })

	err = publication.removeIfUnchanged()
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("replacement readiness cleanup error = %v, want preservation", err)
	}
	got, exists, readErr := readWakeReadyFile(path)
	if readErr != nil || !exists || got != replacement {
		t.Fatalf("replacement readiness = %#v exists=%v err=%v", got, exists, readErr)
	}
}

func TestCrashContractPublicationHooksLeaveRecoverableVisibleStates(t *testing.T) {
	t.Run("target renamed before lock", func(t *testing.T) {
		root, target, _ := newOwnerAcquisitionPublicationFixture(t)
		crash := errors.New("crash after target publication")
		original := publishAuthoritativeWakeAfterTargetRename
		publishAuthoritativeWakeAfterTargetRename = func() { panic(crash) }
		t.Cleanup(func() { publishAuthoritativeWakeAfterTargetRename = original })
		func() {
			defer func() {
				if recovered := recover(); recovered != crash {
					t.Fatalf("target publication panic = %v, want crash sentinel", recovered)
				}
			}()
			_, _ = acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &target, wakeMode: wakeTargetInjectVia,
			})
		}()
		if inspectWakeLock(root, "codex").Exists {
			t.Fatal("target-publication crash exposed a lock")
		}
		persisted, exists, err := readWakeTarget(root, "codex")
		if err != nil || !exists || !sameWakeTarget(persisted, target) {
			t.Fatalf("target-publication crash target=%#v exists=%v err=%v", persisted, exists, err)
		}
	})

	t.Run("lock linked before directory sync", func(t *testing.T) {
		root, target, _ := newOwnerAcquisitionPublicationFixture(t)
		crash := errors.New("crash after lock publication")
		originalLink := publishAuthoritativeWakeLinkAt
		publishAuthoritativeWakeLinkAt = func(
			oldDirFD int,
			oldPath string,
			newDirFD int,
			newPath string,
			flags int,
		) error {
			if err := originalLink(oldDirFD, oldPath, newDirFD, newPath, flags); err != nil {
				return err
			}
			panic(crash)
		}
		t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })
		func() {
			defer func() {
				if recovered := recover(); recovered != crash {
					t.Fatalf("lock publication panic = %v, want crash sentinel", recovered)
				}
			}()
			_, _ = acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &target, wakeMode: wakeTargetInjectVia,
			})
		}()
		inspection := inspectWakeLock(root, "codex")
		if !inspection.Exists || classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
			t.Fatalf("lock-publication crash claim = %#v, want authoritative", inspection)
		}
		persisted, exists, err := readWakeTarget(root, "codex")
		if err != nil || !exists || !sameWakeTarget(persisted, target) {
			t.Fatalf("lock-publication crash target=%#v exists=%v err=%v", persisted, exists, err)
		}
	})

	t.Run("target directory sync fails", func(t *testing.T) {
		root, target, _ := newOwnerAcquisitionPublicationFixture(t)
		syncFailure := errors.New("crash at target directory sync")
		originalSync := syncWakeOwnerDirFD
		syncWakeOwnerDirFD = func(int) error { return syncFailure }
		t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

		_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target: &target, wakeMode: wakeTargetInjectVia,
		})
		if !errors.Is(err, syncFailure) || inspectWakeLock(root, "codex").Exists {
			t.Fatalf("target-sync failure err=%v lock=%#v", err, inspectWakeLock(root, "codex"))
		}
		persisted, exists, readErr := readWakeTarget(root, "codex")
		if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
			t.Fatalf("target-sync failure target=%#v exists=%v err=%v", persisted, exists, readErr)
		}
	})

	t.Run("final lock directory sync fails", func(t *testing.T) {
		root, target, _ := newOwnerAcquisitionPublicationFixture(t)
		syncFailure := errors.New("crash at final lock directory sync")
		originalSync := syncWakeOwnerDirFD
		syncCalls := 0
		syncWakeOwnerDirFD = func(fd int) error {
			syncCalls++
			if syncCalls == 2 {
				return syncFailure
			}
			return originalSync(fd)
		}
		t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

		_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target: &target, wakeMode: wakeTargetInjectVia,
		})
		inspection := inspectWakeLock(root, "codex")
		if !errors.Is(err, syncFailure) ||
			!inspection.Exists || classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
			t.Fatalf("final-sync failure err=%v claim=%#v", err, inspection)
		}
		persisted, exists, readErr := readWakeTarget(root, "codex")
		if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
			t.Fatalf("final-sync failure target=%#v exists=%v err=%v", persisted, exists, readErr)
		}
	})

	t.Run("ready written before validation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wake.ready")
		marker := wakeReady{Schema: wakeReadySchema, Generation: "ready-generation"}
		crash := errors.New("crash after ready publication")
		original := afterWakeReadyPublicationWrite
		afterWakeReadyPublicationWrite = func() { panic(crash) }
		t.Cleanup(func() { afterWakeReadyPublicationWrite = original })
		func() {
			defer func() {
				if recovered := recover(); recovered != crash {
					t.Fatalf("ready publication panic = %v, want crash sentinel", recovered)
				}
			}()
			_, _ = publishWakeReadyFile(path, marker)
		}()
		persisted, exists, err := readWakeReadyFile(path)
		if err != nil || !exists || persisted != marker {
			t.Fatalf("ready-publication crash marker=%#v exists=%v err=%v", persisted, exists, err)
		}
	})
}
