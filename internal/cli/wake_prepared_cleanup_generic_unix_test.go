//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	return withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return cleanupGenericWakeGenerationAt(
			scope,
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
