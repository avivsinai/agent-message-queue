//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
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

func TestWakeSelfUpgradeUnchangedProbeSkipsQuiescence(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidate := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	locator := dir + "/amq"
	if err := os.Symlink(candidate, locator); err != nil {
		t.Fatal(err)
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	previousVersion := wakeSelfUpgradeRunVersion
	wakeSelfUpgradeRunVersion = func(string) (string, error) {
		t.Fatal("unchanged locator ran the version probe")
		return "", nil
	}
	t.Cleanup(func() { wakeSelfUpgradeRunVersion = previousVersion })

	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		selfUpgrade: wakeSelfUpgradeState{
			Enabled:   true,
			Eligible:  true,
			Locator:   locator,
			lastProbe: probe,
		},
		inputDelivery: wakeInputDeliveryState{
			phase:         wakeInputPrimarySubmitPending,
			acceptedBytes: 1,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName)); !os.IsNotExist(err) {
		t.Fatalf("unchanged locator wrote a quiescence decision: %v", err)
	}
}

func TestWakeSelfUpgradeMissingProbeSkipsQuiescence(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(filepath.Join(dir, "missing"), locator); err != nil {
		t.Fatal(err)
	}
	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		selfUpgrade: wakeSelfUpgradeState{
			Enabled:  true,
			Eligible: true,
			Locator:  locator,
		},
		inputDelivery: wakeInputDeliveryState{
			phase:         wakeInputPrimarySubmitPending,
			acceptedBytes: 1,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionNoCandidate)
}

func TestWakeSelfUpgradePostPublicationQuiescenceRaceDefersPendingRecord(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("publish decision=%#v err=%v", decision, err)
	}

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
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if bindCalls != 0 {
		t.Fatalf("post-publication debt bound candidate: calls=%d", bindCalls)
	}
	if !cfg.selfUpgrade.restartPending {
		t.Fatal("deferred self restart lost its process-local pending marker")
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionDeferred)

	cfg.inputDelivery.reset()
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if bindCalls != 1 {
		t.Fatalf("quiescent retry bind calls=%d, want 1", bindCalls)
	}
	if cfg.selfUpgrade.restartPending {
		t.Fatal("refused self restart retained its process-local pending marker")
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
}

func TestWakeSelfUpgradeDiagnosticFailureDoesNotBlockPendingHandler(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	diagnosticPath := filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName)
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}

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
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err == nil {
		t.Fatal("diagnostic publication failure was not reported")
	}
	if bindCalls != 1 {
		t.Fatalf("diagnostic failure blocked pending handler: bind calls=%d", bindCalls)
	}
	if cfg.selfUpgrade.restartPending {
		t.Fatal("persisted refusal retained pending marker")
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
}

func TestWakeSelfUpgradePendingReadFailureRetainsRetryMarker(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartPending
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)
	restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	if err := os.Remove(restartPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(restartPath, 0o700); err != nil {
		t.Fatal(err)
	}

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
		selfUpgrade: wakeSelfUpgradeState{
			Enabled:        true,
			Eligible:       true,
			restartPending: true,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err == nil {
		t.Fatal("pending record read failure was not reported")
	}
	if !cfg.selfUpgrade.restartPending || bindCalls != 0 {
		t.Fatalf("transient read changed pending state: pending=%v binds=%d", cfg.selfUpgrade.restartPending, bindCalls)
	}

	if err := os.Remove(restartPath); err != nil {
		t.Fatal(err)
	}
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.restartPending {
		t.Fatalf("pending retry = binds=%d pending=%v, want handled refusal", bindCalls, cfg.selfUpgrade.restartPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
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
