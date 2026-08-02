//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishWakeStateInstallsCanonical0600Snapshot(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	snapshot, err := publishWakeStateForTest(fixture, expected)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.FileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 0600", got)
	}
	canonical, err := encodeWakeState(snapshot.State)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot.Raw, canonical) {
		t.Fatalf("installed bytes = %q, want canonical %q", snapshot.Raw, canonical)
	}
	if err := validateWakeStateAgainstLegacy(snapshot.State, expected.legacy()); err != nil {
		t.Fatalf("installed state does not mirror captured legacy: %v", err)
	}
}

func TestPublishWakeStateRefusesSymlinkDestination(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	target := filepath.Join(fixture.root, "outside-state")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := publishWakeStateForTest(fixture, expected); err == nil {
		t.Fatal("symlink state destination was accepted")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("symlink destination was removed: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink destination was replaced")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: bytes=%q error=%v", got, err)
	}
}

func TestPublishWakeStateCrashSeamsExposeOnlyOldOrNewState(t *testing.T) {
	tests := []struct {
		boundary wakeStatePublicationBoundary
		wantArg  string
	}{
		{boundary: wakeStateAfterTempWrite, wantArg: "old"},
		{boundary: wakeStateAfterFileSync, wantArg: "old"},
		{boundary: wakeStateAfterPreRenameDirSync, wantArg: "old"},
		{boundary: wakeStateAfterRename, wantArg: "new"},
		{boundary: wakeStateAfterPostRenameDirSync, wantArg: "new"},
		{boundary: wakeStateAfterVerify, wantArg: "new"},
	}
	for _, test := range tests {
		t.Run(string(test.boundary), func(t *testing.T) {
			fixture := newWakeStateUnixFixture(t, "old")
			oldLegacy := captureWakeStateLegacyForTest(t, fixture)
			if _, err := publishWakeStateForTest(fixture, oldLegacy); err != nil {
				t.Fatal(err)
			}

			writeWakeStateTargetForTest(t, fixture, "new")
			newLegacy := captureWakeStateLegacyForTest(t, fixture)
			injected := errors.New("simulated publication crash")
			originalHook := afterWakeStatePublicationBoundary
			afterWakeStatePublicationBoundary = func(boundary wakeStatePublicationBoundary) error {
				if boundary == test.boundary {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { afterWakeStatePublicationBoundary = originalHook })

			if _, err := publishWakeStateForTest(fixture, newLegacy); !errors.Is(err, injected) {
				t.Fatalf("publication error = %v, want injected crash", err)
			}
			afterWakeStatePublicationBoundary = originalHook

			current := readWakeStateForTest(t, fixture)
			if got := current.State.Target.InjectArgs[0]; got != test.wantArg {
				t.Fatalf("visible state arg = %q, want %q", got, test.wantArg)
			}
			assertNoWakeStateTemps(t, fixture.agentDir.path)
		})
	}
}

func TestReadWakeStateSnapshotClassifiesReplacementAsTypedChange(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	snapshot, err := publishWakeStateForTest(fixture, expected)
	if err != nil {
		t.Fatal(err)
	}

	originalHook := afterWakeStateSnapshotRead
	replaced := false
	afterWakeStateSnapshotRead = func() {
		if replaced {
			return
		}
		replaced = true
		temp := filepath.Join(fixture.agentDir.path, ".wake-state-replacement")
		if err := os.WriteFile(temp, snapshot.Raw, 0o600); err != nil {
			t.Errorf("write replacement: %v", err)
			return
		}
		if err := os.Rename(temp, filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
			t.Errorf("install replacement: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateSnapshotRead = originalHook })

	var readErr error
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, _, readErr = readWakeStateSnapshotAt(dirfd, fixture.agentDir)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var changed *wakeSnapshotReadChangedError
	if !errors.As(readErr, &changed) {
		t.Fatalf("read error = %v, want typed snapshot change", readErr)
	}
	if _, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
		t.Fatalf("replacement was not preserved: %v", err)
	}
}

func TestReadInvalidWakeStateDoesNotMutateArtifact(t *testing.T) {
	tests := []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{name: "invalid", raw: func(*testing.T) []byte { return []byte("{}\n") }},
		{name: "newer schema", raw: func(t *testing.T) []byte {
			state, _ := validWakeStateFixture(t, true)
			state.Schema = 2
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeStateUnixFixture(t, "initial")
			path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			invalid := test.raw(t)
			if err := os.WriteFile(path, invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			var readErr error
			if err := fixture.agentDir.withFD(func(dirfd int) error {
				_, _, readErr = readWakeStateSnapshotAt(dirfd, fixture.agentDir)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if readErr == nil {
				t.Fatal("invalid wake state was accepted")
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatalf("invalid state was removed: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !bytes.Equal(got, invalid) {
				t.Fatal("invalid state was rewritten or replaced by a read")
			}
		})
	}
}

func TestRemoveWakeStateIfSnapshotMatchesRemovesExactSnapshot(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	snapshot, err := publishWakeStateForTest(fixture, expected)
	if err != nil {
		t.Fatal(err)
	}

	var removed bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var err error
		removed, err = removeWakeStateIfSnapshotMatchesAt(dirfd, fixture.agentDir, snapshot)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("exact wake state snapshot was not removed")
	}
	if _, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("removed wake state stat error = %v, want not exist", err)
	}
}

func TestRemoveWakeStateIfSnapshotMatchesPreservesReplacement(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	snapshot, err := publishWakeStateForTest(fixture, expected)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	replacementRaw := bytes.Clone(snapshot.Raw)
	temp := filepath.Join(fixture.agentDir.path, ".wake-state-replacement")
	if err := os.WriteFile(temp, replacementRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var removed bool
	var removeErr error
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		removed, removeErr = removeWakeStateIfSnapshotMatchesAt(dirfd, fixture.agentDir, snapshot)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if removed || removeErr == nil || !strings.Contains(removeErr.Error(), "preserving") {
		t.Fatalf("removed=%v error=%v, want replacement preservation", removed, removeErr)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("replacement was removed: %v", err)
	}
	if !os.SameFile(replacementInfo, after) {
		t.Fatal("replacement identity changed during refused cleanup")
	}
}

func TestRemoveWakeStatePreservesReboundSiblingDirectory(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "initial")
	expected := captureWakeStateLegacyForTest(t, fixture)
	snapshot, err := publishWakeStateForTest(fixture, expected)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := fixture.agentDir.path
	detachedPath := originalPath + ".detached"
	if err := os.Rename(originalPath, detachedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(originalPath, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingRaw := []byte("sibling")
	siblingPath := filepath.Join(originalPath, wakeStateFileName)
	if err := os.WriteFile(siblingPath, siblingRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	var removeErr error
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, removeErr = removeWakeStateIfSnapshotMatchesAt(dirfd, fixture.agentDir, snapshot)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if removeErr == nil || !strings.Contains(removeErr.Error(), "canonical wake agent directory") {
		t.Fatalf("rebound cleanup error = %v, want canonical-directory refusal", removeErr)
	}
	got, err := os.ReadFile(siblingPath)
	if err != nil {
		t.Fatalf("rebound sibling was removed: %v", err)
	}
	if !bytes.Equal(got, siblingRaw) {
		t.Fatalf("rebound sibling bytes = %q, want %q", got, siblingRaw)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, wakeStateFileName)); err != nil {
		t.Fatalf("detached original state was removed: %v", err)
	}
}

type wakeStateUnixFixture struct {
	root     string
	agent    string
	injector string
	agentDir *wakeAgentDir
}

func newWakeStateUnixFixture(t *testing.T, arg string) wakeStateUnixFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	injector := filepath.Join(root, "injector")
	if err := os.WriteFile(injector, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := wakeStateUnixFixture{root: root, agent: "codex", injector: injector}
	writeWakeStateTargetForTest(t, fixture, arg)
	agentDir, err := openWakeAgentDir(root, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	fixture.agentDir = agentDir
	t.Cleanup(func() { _ = agentDir.Close() })
	return fixture
}

func writeWakeStateTargetForTest(t *testing.T, fixture wakeStateUnixFixture, arg string) {
	t.Helper()
	target := wakeTarget{
		Schema:     wakeTargetSchema,
		Mode:       wakeTargetInjectVia,
		Root:       canonicalWakeRoot(fixture.root),
		Agent:      fixture.agent,
		Created:    "2026-08-02T00:00:00Z",
		InjectVia:  fixture.injector,
		InjectArgs: []string{arg},
	}
	if err := writeWakeTarget(fixture.root, fixture.agent, target); err != nil {
		t.Fatal(err)
	}
}

func captureWakeStateLegacyForTest(t *testing.T, fixture wakeStateUnixFixture) wakeStateLegacySnapshot {
	t.Helper()
	var snapshot wakeStateLegacySnapshot
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		var err error
		snapshot, err = captureWakeStateLegacySnapshotAt(
			dirfd,
			fixture.agentDir,
			fixture.root,
			fixture.agent,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func publishWakeStateForTest(
	fixture wakeStateUnixFixture,
	expected wakeStateLegacySnapshot,
) (wakeStateFileSnapshot, error) {
	var snapshot wakeStateFileSnapshot
	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		var err error
		snapshot, err = publishWakeStateAt(
			dirfd,
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			expected,
		)
		return err
	})
	return snapshot, err
}

func readWakeStateForTest(t *testing.T, fixture wakeStateUnixFixture) wakeStateFileSnapshot {
	t.Helper()
	var snapshot wakeStateFileSnapshot
	var exists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var err error
		snapshot, exists, err = readWakeStateSnapshotAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("wake state is missing")
	}
	return snapshot
}

func assertNoWakeStateTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wake-state.tmp.") {
			t.Fatalf("publication temp survived: %s", entry.Name())
		}
	}
}
