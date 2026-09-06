//go:build darwin || linux

package cli

import (
	"errors"
	"path/filepath"
	"testing"
)

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
		if inspection.Lock.StateGeneration != inspection.Lock.Generation ||
			inspection.Lock.StateDigest != inspection.Lock.TargetDigest {
			t.Fatalf("lock-publication crash lost state binding: %#v", inspection.Lock)
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
		originalSync := syncAuthoritativeWakeLockAfterCommitDirFD
		syncAuthoritativeWakeLockAfterCommitDirFD = func(int) error {
			return syncFailure
		}
		t.Cleanup(func() { syncAuthoritativeWakeLockAfterCommitDirFD = originalSync })

		_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target: &target, wakeMode: wakeTargetInjectVia,
		})
		inspection := inspectWakeLock(root, "codex")
		if !errors.Is(err, syncFailure) ||
			!inspection.Exists || classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
			t.Fatalf("final-sync failure err=%v claim=%#v", err, inspection)
		}
		if inspection.Lock.StateGeneration != inspection.Lock.Generation ||
			inspection.Lock.StateDigest != inspection.Lock.TargetDigest {
			t.Fatalf("final-sync crash lost state binding: %#v", inspection.Lock)
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
