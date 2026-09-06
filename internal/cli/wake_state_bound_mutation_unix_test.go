//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBoundPreparedPublicationGapAllowsPublicWakeRestart(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	installWakeStateMutationForTest(t, statePath, func(state *wakeState) {
		state.Prepared = nil
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, fixture.options)
	if err != nil {
		t.Fatalf("restart after prepared publication gap: %v", err)
	}
	t.Cleanup(cleanup)
	restarted := inspectWakeLock(fixture.root, fixture.me)
	if sameWakeLockGeneration(fixture.created, restarted) {
		t.Fatal("restart retained the stale wake generation")
	}
	if err := writeWakePreparedFile(fixture.root, fixture.me, restarted); err != nil {
		t.Fatalf("publish restarted wake preparation: %v", err)
	}
	state := readWakeStateAtPathForTest(t, fixture.root, fixture.me)
	if state.State.Prepared == nil ||
		state.State.Prepared.Generation != restarted.Lock.Generation ||
		state.State.Prepared.TargetDigest != restarted.Lock.TargetDigest {
		t.Fatalf("restarted prepared projection = %#v, lock = %#v", state.State.Prepared, restarted.Lock)
	}
}

func installWakeStateMutationForTest(t *testing.T, path string, mutate func(*wakeState)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeWakeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&state)
	raw, err = encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
