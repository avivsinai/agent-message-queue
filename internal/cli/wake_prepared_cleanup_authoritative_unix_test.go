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
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return publishAuthoritativeWakeClaimAt(scope, root, me, target, lock)
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
	return withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return removeAuthoritativeWakeClaimAt(
			scope,
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
