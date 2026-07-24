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
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type genericWakePreparedCleanupFixture struct {
	root           string
	me             string
	agentDir       *wakeAgentDir
	created        wakeLockInspection
	options        wakeLockAcquireOptions
	target         *wakeTarget
	preparedPath   string
	preparedMarker wakeReady
	preparedRaw    []byte
}

func TestGenericWakeCleanupRemovesOwnPreparedMarker(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	fixture.assertLockMissing(t)
	if _, err := os.Stat(fixture.preparedPath); !os.IsNotExist(err) {
		t.Fatalf("own prepared marker survived cleanup: %v", err)
	}
}

func TestGenericWakeCleanupMissingAndWrongGenerationPreparedAreSafe(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fixture := newGenericWakePreparedCleanupFixture(t, false)
		if err := os.Remove(fixture.preparedPath); err != nil {
			t.Fatal(err)
		}
		if err := fixture.cleanupNow(); err != nil {
			t.Fatal(err)
		}
		fixture.assertLockMissing(t)
		if _, err := os.Stat(fixture.preparedPath); !os.IsNotExist(err) {
			t.Fatalf("missing prepared marker was recreated: %v", err)
		}
	})

	t.Run("wrong generation", func(t *testing.T) {
		fixture := newGenericWakePreparedCleanupFixture(t, false)
		replacement := fixture.preparedMarker
		replacement.Generation = "replacement-generation"
		replacementRaw := writeGenericPreparedMarkerForTest(
			t,
			fixture.preparedPath,
			replacement,
		)
		if err := fixture.cleanupNow(); err != nil {
			t.Fatal(err)
		}
		fixture.assertLockMissing(t)
		assertFileRawForTest(t, fixture.preparedPath, replacementRaw)
	})
}

func TestGenericWakeCleanupInvalidPreparedStillRemovesExactLock(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	invalid := []byte("{not-json\n")
	if err := os.WriteFile(fixture.preparedPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	err := fixture.cleanupNow()
	if err == nil || !strings.Contains(err.Error(), "snapshot wake prepared marker") {
		t.Fatalf("invalid prepared cleanup error = %v, want snapshot error", err)
	}
	fixture.assertLockMissing(t)
	assertFileRawForTest(t, fixture.preparedPath, invalid)
}

func TestGenericWakeCleanupAlwaysSyncsExactLockRemoval(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *genericWakePreparedCleanupFixture)
	}{
		{
			name: "missing prepared",
			mutate: func(t *testing.T, fixture *genericWakePreparedCleanupFixture) {
				t.Helper()
				if err := os.Remove(fixture.preparedPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong generation prepared",
			mutate: func(t *testing.T, fixture *genericWakePreparedCleanupFixture) {
				t.Helper()
				replacement := fixture.preparedMarker
				replacement.Generation = "replacement-generation"
				writeGenericPreparedMarkerForTest(t, fixture.preparedPath, replacement)
			},
		},
		{
			name: "malformed prepared",
			mutate: func(t *testing.T, fixture *genericWakePreparedCleanupFixture) {
				t.Helper()
				if err := os.WriteFile(fixture.preparedPath, []byte("{not-json\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGenericWakePreparedCleanupFixture(t, false)
			test.mutate(t, fixture)
			syncCalls := installGenericWakeCleanupDirSync(t, nil)

			err := fixture.cleanupNow()
			if test.name == "malformed prepared" {
				if err == nil || !strings.Contains(err.Error(), "snapshot wake prepared marker") {
					t.Fatalf("malformed prepared cleanup error = %v, want snapshot error", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if *syncCalls != 1 {
				t.Fatalf("directory sync calls = %d, want exact lock-removal sync", *syncCalls)
			}
			fixture.assertLockMissing(t)
		})
	}
}

func TestGenericWakeCleanupLockSyncFailureContinuesPreparedAndFloorCleanup(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	floor, err := newWakeRepairFloor(
		fixture.root,
		fixture.me,
		fixture.created.Lock,
		*fixture.target,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWakeRepairFloor(fixture.root, fixture.me, floor); err != nil {
		t.Fatal(err)
	}
	syncCalls := installGenericWakeCleanupDirSync(t, func(call int) error {
		if call == 1 {
			return syscall.ENOSYS
		}
		return nil
	})

	err = fixture.cleanupNow()
	if err == nil || !strings.Contains(err.Error(), "sync exact generic wake lock removal") {
		t.Fatalf("lock sync cleanup error = %v, want durability error", err)
	}
	if *syncCalls != 3 {
		t.Fatalf("directory sync calls = %d, want lock, prepared, and floor syncs", *syncCalls)
	}
	fixture.assertLockMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
	assertPathMissingForTest(t, wakeRepairFloorPath(fixture.root, fixture.me))
}

func TestGenericWakeCleanupPreservesPreparedMarkerMutations(t *testing.T) {
	t.Run("new inode", func(t *testing.T) {
		fixture := newGenericWakePreparedCleanupFixture(t, false)
		replacementPath := fixture.preparedPath + ".replacement"
		if err := os.WriteFile(replacementPath, fixture.preparedRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		installGenericWakeCleanupInterleave(t, func(int, *wakeAgentDir) error {
			return os.Rename(replacementPath, fixture.preparedPath)
		})

		err := fixture.cleanupNow()
		if err == nil || !strings.Contains(err.Error(), "preserving") {
			t.Fatalf("new-inode prepared cleanup error = %v, want preservation", err)
		}
		fixture.assertLockMissing(t)
		assertFileRawForTest(t, fixture.preparedPath, fixture.preparedRaw)
	})

	t.Run("same inode raw mutation", func(t *testing.T) {
		fixture := newGenericWakePreparedCleanupFixture(t, false)
		before, err := os.Stat(fixture.preparedPath)
		if err != nil {
			t.Fatal(err)
		}
		replacement := fixture.preparedMarker
		replacement.Generation = "same-inode-replacement"
		replacementRaw, err := json.Marshal(replacement)
		if err != nil {
			t.Fatal(err)
		}
		replacementRaw = append(replacementRaw, '\n')
		installGenericWakeCleanupInterleave(t, func(int, *wakeAgentDir) error {
			return os.WriteFile(fixture.preparedPath, replacementRaw, 0o600)
		})

		err = fixture.cleanupNow()
		if err == nil || !strings.Contains(err.Error(), "preserving") {
			t.Fatalf("same-inode prepared cleanup error = %v, want preservation", err)
		}
		fixture.assertLockMissing(t)
		after, err := os.Stat(fixture.preparedPath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Fatal("same-inode mutation unexpectedly replaced the marker inode")
		}
		assertFileRawForTest(t, fixture.preparedPath, replacementRaw)
	})
}

func TestGenericWakeCleanupReplacementLockPreservesReplacementAndCleansOldFloor(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	floor, err := newWakeRepairFloor(
		fixture.root,
		fixture.me,
		fixture.created.Lock,
		*fixture.target,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWakeRepairFloor(fixture.root, fixture.me, floor); err != nil {
		t.Fatal(err)
	}

	replacementLock := fixture.created.Lock
	replacementLock.Generation = "replacement-lock-generation"
	replacementLock.Started = time.Now().UTC().Format(time.RFC3339)
	replacementLockRaw, err := json.Marshal(replacementLock)
	if err != nil {
		t.Fatal(err)
	}
	replacementLockRaw = append(replacementLockRaw, '\n')
	replacementMarker := fixture.preparedMarker
	replacementMarker.Generation = replacementLock.Generation
	replacementMarkerRaw, err := json.Marshal(replacementMarker)
	if err != nil {
		t.Fatal(err)
	}
	replacementMarkerRaw = append(replacementMarkerRaw, '\n')
	lockPath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock")
	installGenericWakeCleanupInterleave(t, func(int, *wakeAgentDir) error {
		if err := os.WriteFile(lockPath, replacementLockRaw, 0o600); err != nil {
			return err
		}
		replacementPath := fixture.preparedPath + ".replacement"
		if err := os.WriteFile(replacementPath, replacementMarkerRaw, 0o600); err != nil {
			return err
		}
		return os.Rename(replacementPath, fixture.preparedPath)
	})

	err = fixture.cleanupNow()
	if err == nil || !strings.Contains(err.Error(), "replacement wake lock appeared") {
		t.Fatalf("replacement cleanup error = %v, want replacement preservation", err)
	}
	assertFileRawForTest(t, lockPath, replacementLockRaw)
	assertFileRawForTest(t, fixture.preparedPath, replacementMarkerRaw)
	if _, err := os.Stat(wakeRepairFloorPath(fixture.root, fixture.me)); !os.IsNotExist(err) {
		t.Fatalf("old exact repair floor survived replacement interleaving: %v", err)
	}
}

func TestGenericWakeCleanupAcceptExistingNeverAcquiresCleanupAuthority(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: fixture.created.Lock.ProcessStart,
			BootID:     fixture.created.Lock.BootID,
			Executable: "amq",
			Args: []string{
				"amq",
				"wake",
				"--root", fixture.root,
				"--me", fixture.me,
			},
		}
	})

	reusedCleanup, err := acquireWakeLockWithOptions(
		fixture.root,
		fixture.me,
		wakeLockAcquireOptions{
			acceptExistingValid: true,
			wakeMode:            fixture.created.Lock.WakeMode,
		},
	)
	var alreadyRunning *wakeAlreadyRunningError
	if !errors.As(err, &alreadyRunning) {
		t.Fatalf("accept-existing result = %v, want already running", err)
	}
	if reusedCleanup != nil {
		t.Fatal("accept-existing caller received cleanup authority")
	}
	current := inspectWakeLock(fixture.root, fixture.me)
	if !sameWakeLockGeneration(fixture.created, current) {
		t.Fatalf("accept-existing changed wake lock: before=%#v after=%#v", fixture.created, current)
	}
	assertFileRawForTest(t, fixture.preparedPath, fixture.preparedRaw)

	if err := fixture.cleanupNow(); err != nil {
		t.Fatal(err)
	}
	fixture.assertLockMissing(t)
	if _, err := os.Stat(fixture.preparedPath); !os.IsNotExist(err) {
		t.Fatalf("own cleanup left prepared marker after accept-existing refusal: %v", err)
	}
}

func newGenericWakePreparedCleanupFixture(
	t *testing.T,
	withTarget bool,
) *genericWakePreparedCleanupFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	const me = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatal(err)
	}
	options := wakeLockAcquireOptions{wakeMode: wakeInjectModeNone}
	var target *wakeTarget
	if withTarget {
		injectorPath := filepath.Join(root, "test-injector")
		if err := os.WriteFile(injectorPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		value, err := newWakeTarget(root, me, injectorPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		target = &value
		options.target = target
	}
	fallbackCleanup, err := acquireWakeLockWithOptions(root, me, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fallbackCleanup)
	created := inspectWakeLock(root, me)
	if !created.Exists || created.Lock.Generation == "" {
		t.Fatalf("created wake lock = %#v", created)
	}
	if err := writeWakePreparedFile(root, me, created); err != nil {
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
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	return &genericWakePreparedCleanupFixture{
		root:           root,
		me:             me,
		agentDir:       agentDir,
		created:        created,
		options:        options,
		target:         target,
		preparedPath:   preparedPath,
		preparedMarker: marker,
		preparedRaw:    preparedRaw,
	}
}

func (fixture *genericWakePreparedCleanupFixture) cleanupNow() error {
	return withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return cleanupGenericWakeGenerationAt(
			dirfd,
			fixture.agentDir,
			fixture.root,
			fixture.me,
			fixture.created,
			fixture.options,
		)
	})
}

func (fixture *genericWakePreparedCleanupFixture) assertLockMissing(t *testing.T) {
	t.Helper()
	lockPath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("exact generic wake lock survived cleanup: %v", err)
	}
}

func installGenericWakeCleanupInterleave(
	t *testing.T,
	fn func(int, *wakeAgentDir) error,
) {
	t.Helper()
	original := afterGenericWakeLockRemoval
	afterGenericWakeLockRemoval = fn
	t.Cleanup(func() { afterGenericWakeLockRemoval = original })
}

func installGenericWakeCleanupDirSync(
	t *testing.T,
	fail func(call int) error,
) *int {
	t.Helper()
	original := syncWakeOwnerDirFD
	calls := 0
	syncWakeOwnerDirFD = func(int) error {
		calls++
		if fail != nil {
			return fail(calls)
		}
		return nil
	}
	t.Cleanup(func() { syncWakeOwnerDirFD = original })
	return &calls
}

func writeGenericPreparedMarkerForTest(
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

func assertFileRawForTest(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s raw = %q, want %q", path, got, want)
	}
}
