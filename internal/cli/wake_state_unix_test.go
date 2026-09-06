//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
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
	err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		var err error
		snapshot, err = publishWakeStateAt(
			scope,
			fixture.root,
			fixture.agent,
			expected,
		)
		return err
	})
	return snapshot, err
}
