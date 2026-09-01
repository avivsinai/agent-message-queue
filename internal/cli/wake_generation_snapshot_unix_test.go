//go:build darwin || linux

package cli

import (
	"encoding/json"
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

func TestRemoveWakeGenerationFileIfSnapshotMatchesRemovesOwnSnapshot(t *testing.T) {
	fixture := newWakeGenerationSnapshotFixture(t)
	removed, err := removeWakeGenerationSnapshotForTest(t, fixture, fixture.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("exact generation snapshot was not removed")
	}
	if _, err := os.Stat(filepath.Join(fixture.agentDir.path, testWakeGenerationSnapshotName)); !os.IsNotExist(err) {
		t.Fatalf("removed generation file stat error = %v, want not exist", err)
	}
}

func TestRemoveWakeGenerationFileIfSnapshotMatchesMissingIsNoop(t *testing.T) {
	fixture := newWakeGenerationSnapshotFixture(t)
	path := filepath.Join(fixture.agentDir.path, testWakeGenerationSnapshotName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	removed, err := removeWakeGenerationSnapshotForTest(t, fixture, fixture.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("missing generation file reported removal")
	}
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

func TestRemoveWakeGenerationFileIfSnapshotMatchesPreservesSameInodeRawMutation(t *testing.T) {
	fixture := newWakeGenerationSnapshotFixture(t)
	replacement := fixture.marker
	replacement.Generation = "replacement-generation"
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.agentDir.path, testWakeGenerationSnapshotName)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fixture.snapshot.FileInfo, currentInfo) {
		t.Fatal("raw mutation replaced the inode instead of mutating it")
	}

	removed, err := removeWakeGenerationSnapshotForTest(t, fixture, fixture.snapshot)
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("same-inode mutation removal error = %v, want preservation", err)
	}
	if removed {
		t.Fatal("same-inode raw mutation was removed")
	}
	assertWakeGenerationMarkerForTest(t, fixture, replacement)
}

func TestRemoveWakeGenerationFileIfSnapshotMatchesPreservesSemanticDigestMismatch(t *testing.T) {
	fixture := newWakeGenerationSnapshotFixture(t)
	mismatched := fixture.snapshot
	mismatched.Marker.TargetDigest = "sha256:" + strings.Repeat("b", 64)

	removed, err := removeWakeGenerationSnapshotForTest(t, fixture, mismatched)
	if err == nil || !strings.Contains(err.Error(), "semantics changed") {
		t.Fatalf("semantic digest mismatch removal error = %v, want semantic preservation", err)
	}
	if removed {
		t.Fatal("semantic digest mismatch was removed")
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
