//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testWakeGenerationSnapshotName = ".wake.prepared.snapshot-test"

type wakeGenerationSnapshotFixture struct {
	agentDir *wakeAgentDir
	label    string
	marker   wakeReady
	snapshot wakeGenerationFileSnapshot
}

func TestRemoveWakeGenerationFileIfSnapshotMatchesPreservesNewInodeReplacement(t *testing.T) {
	fixture := newWakeGenerationSnapshotFixture(t)
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeGenerationFileAt(
			scope,
			testWakeGenerationSnapshotName,
			fixture.label,
			fixture.marker,
		)
	}); err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(filepath.Join(fixture.agentDir.path, testWakeGenerationSnapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(fixture.snapshot.FileInfo, currentInfo) {
		t.Fatal("replacement unexpectedly retained the original inode")
	}

	removed, err := removeWakeGenerationSnapshotForTest(t, fixture, fixture.snapshot)
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("new-inode replacement removal error = %v, want preservation", err)
	}
	if removed {
		t.Fatal("new-inode replacement was removed")
	}
	assertWakeGenerationMarkerForTest(t, fixture, fixture.marker)
}

func newWakeGenerationSnapshotFixture(t *testing.T) wakeGenerationSnapshotFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	fixture := wakeGenerationSnapshotFixture{
		agentDir: agentDir,
		label:    "wake prepared snapshot test",
		marker: wakeReady{
			Schema:       wakeReadySchema,
			Generation:   "snapshot-generation",
			TargetDigest: "sha256:" + strings.Repeat("a", 64),
		},
	}
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return writeWakeGenerationFileAt(
			scope,
			testWakeGenerationSnapshotName,
			fixture.label,
			fixture.marker,
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := agentDir.withFD(func(dirfd int) error {
		var readErr error
		fixture.snapshot, _, readErr = readWakeGenerationFileSnapshotAt(
			dirfd,
			agentDir,
			testWakeGenerationSnapshotName,
			fixture.label,
		)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if fixture.snapshot.FileInfo == nil {
		t.Fatal("generation snapshot file identity is missing")
	}
	return fixture
}

func removeWakeGenerationSnapshotForTest(
	t *testing.T,
	fixture wakeGenerationSnapshotFixture,
	expected wakeGenerationFileSnapshot,
) (bool, error) {
	t.Helper()
	var removed bool
	var removeErr error
	err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		removed, removeErr = removeWakeGenerationFileIfSnapshotMatchesAt(
			scope,
			testWakeGenerationSnapshotName,
			fixture.label,
			expected,
		)
		return removeErr
	})
	if err != nil && removeErr == nil {
		return false, err
	}
	return removed, removeErr
}

func assertWakeGenerationMarkerForTest(
	t *testing.T,
	fixture wakeGenerationSnapshotFixture,
	want wakeReady,
) {
	t.Helper()
	var marker wakeReady
	var exists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var readErr error
		marker, exists, readErr = readWakeGenerationFileAt(
			dirfd,
			fixture.agentDir,
			testWakeGenerationSnapshotName,
			fixture.label,
		)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if !exists || marker != want {
		t.Fatalf("preserved marker = %#v exists=%v, want %#v", marker, exists, want)
	}
}
