//go:build darwin || linux

package cli

import (
	"testing"
)

func TestWakeStateDualReadPrefersExactState(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "legacy")
	publishWakeStateForDualReadTest(t, fixture)

	selection, err := readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StatePreferred {
		t.Fatal("exact state was not preferred")
	}
	if got := selection.Target.InjectArgs; len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("target args = %v", got)
	}
}

func readWakeStateSelectionForTest(t *testing.T, fixture wakeStateUnixFixture) (wakeStateReadSelection, error) {
	t.Helper()
	var selection wakeStateReadSelection
	err := fixture.agentDir.withFD(func(dirfd int) error {
		var err error
		selection, err = readWakeStateSelectionAt(
			dirfd,
			fixture.agentDir,
			fixture.root,
			fixture.agent,
		)
		return err
	})
	return selection, err
}

func publishWakeStateForDualReadTest(t *testing.T, fixture wakeStateUnixFixture) {
	t.Helper()
	expected := captureWakeStateLegacyForTest(t, fixture)
	if _, err := publishWakeStateForTest(fixture, expected); err != nil {
		t.Fatal(err)
	}
}
