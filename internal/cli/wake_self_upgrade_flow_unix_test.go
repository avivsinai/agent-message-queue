//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"testing"
)

func TestWakeSelfUpgradeQuiescenceDefersThenRetries(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	bindCalls := 0
	previousBind := wakeRestartBind
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		return nil, errors.New("test bind refusal")
	}
	t.Cleanup(func() { wakeRestartBind = previousBind })

	cfg := wakeConfig{
		me:                   fixture.agent,
		root:                 fixture.root,
		injectMode:           wakeInjectModeNone,
		wakeOwner:            &fixture.owner,
		terminalGeneration:   fixture.lock.Lock.Generation,
		terminalImageVersion: fixture.lock.Lock.ImageVersion,
		retainedAgent:        fixture.agentDir,
		retainedInbox:        fixture.inboxDir,
		selfUpgrade:          state,
		inputDelivery: wakeInputDeliveryState{
			phase:         wakeInputPrimarySubmitPending,
			acceptedBytes: 1,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 0 {
		t.Fatalf("self-upgrade bound during delivery debt: bind calls=%d", bindCalls)
	}
	if _, err := os.Lstat(fixture.agentDir.path + "/" + wakeRestartFileName); !os.IsNotExist(err) {
		t.Fatalf("quiescence deferral published a restart record: %v", err)
	}
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionDeferred)

	cfg.inputDelivery.reset()
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 {
		t.Fatalf("quiescent self-upgrade bind calls=%d, want 1", bindCalls)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefused)
}

func assertWakeSelfUpgradeRecordStatus(t *testing.T, fixture wakeRestartFixture, status string) {
	t.Helper()
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		record, exists, err := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists || record.Source != wakeRestartSourceSelf || record.Status != status {
			return errors.New("unexpected wake self-upgrade record")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertWakeSelfUpgradeDiagnosticAction(t *testing.T, fixture wakeRestartFixture, action string) {
	t.Helper()
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		diagnostic, exists := readWakeSelfUpgradeDiagnosticAt(dirfd, fixture.agentDir, fixture.lock)
		if !exists || diagnostic.LastDecision.Action != action {
			return errors.New("unexpected wake self-upgrade diagnostic")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
