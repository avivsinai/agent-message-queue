//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestWakeSelfUpgradePostWriteReadFailureAdoptsPersistedRecord(t *testing.T) {
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
	previousReadPublished := wakeSelfUpgradeReadPublished
	wakeSelfUpgradeReadPublished = func(int, *wakeAgentDir) (wakeRestartRecord, bool, error) {
		return wakeRestartRecord{}, false, errors.New("test post-write read failure")
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		wakeSelfUpgradeReadPublished = previousReadPublished
	})

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
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err == nil {
		t.Fatal("post-write read failure was not reported")
	}
	if bindCalls != 0 || cfg.selfUpgrade.restartPending {
		t.Fatalf("failed publication handled early: binds=%d pending=%v", bindCalls, cfg.selfUpgrade.restartPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)

	wakeSelfUpgradeReadPublished = previousReadPublished
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.restartPending || cfg.selfUpgrade.refusalPending != nil {
		t.Fatalf(
			"adopted publication = binds=%d pending=%v refusal=%#v",
			bindCalls,
			cfg.selfUpgrade.restartPending,
			cfg.selfUpgrade.refusalPending,
		)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
}

func TestPublishWakeSelfUpgradePendingAdoptsOnlyExactSelfRecord(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *wakeRestartRecord)
		wantAction string
	}{
		{name: "exact self pending", wantAction: wakeSelfUpgradeActionPending},
		{
			name: "foreign pending",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Source = wakeRestartSourceForeign
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "claimed successor",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Schema = wakeRestartSchemaV2
				record.SuccessorGeneration = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "different owner",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Owner.SessionID++
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "different root",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Root += "-other"
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "different agent",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Agent = "claude"
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "different generation",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
		{
			name: "different candidate",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Candidate.EmbeddedVersion = "0.57.1"
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		},
	}
	if runtime.GOOS == "darwin" {
		tests = append(tests, struct {
			name       string
			mutate     func(*testing.T, *wakeRestartRecord)
			wantAction string
		}{
			name: "different previous bound image",
			mutate: func(t *testing.T, record *wakeRestartRecord) {
				t.Helper()
				previous := record.Candidate
				stagePath, err := planWakeRestartStagePlatform(
					previous,
					"cccccccccccccccccccccccccccccccc",
				)
				if err != nil {
					t.Fatal(err)
				}
				previous.ExecutionPath = stagePath
				previous.Method = wakeImageMethodPathnameExecObserved
				record.PreviousBoundImage = &previous
			},
			wantAction: wakeSelfUpgradeActionRestartPending,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			existing := fixture.record
			existing.Source = wakeRestartSourceSelf
			if test.mutate != nil {
				test.mutate(t, &existing)
			}
			writeWakeCheckSelfUpgradeRestartRecord(t, fixture, existing)

			proposed := fixture.record
			proposed.Source = wakeRestartSourceSelf
			proposed.RequestID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			decision, err := publishWakeSelfUpgradePending(
				fixture.agentDir,
				fixture.lock,
				proposed,
				wakeSelfUpgradeDecision{
					Candidate: wakeSelfUpgradeCandidateFromEvidence(proposed.Candidate),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != test.wantAction {
				t.Fatalf("decision=%#v, want action %q", decision, test.wantAction)
			}
		})
	}
}

func TestWakeSelfUpgradePreInstallPublicationFailureRetriesCandidate(t *testing.T) {
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
	previousSync := syncWakeOwnerDirFD
	failNextSync := true
	syncWakeOwnerDirFD = func(fd int) error {
		if failNextSync {
			failNextSync = false
			return errors.New("test pre-install sync failure")
		}
		return previousSync(fd)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		syncWakeOwnerDirFD = previousSync
	})

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
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err == nil {
		t.Fatal("pre-install publication failure was not reported")
	}
	if bindCalls != 0 {
		t.Fatalf("pre-install failure bound candidate %d time(s)", bindCalls)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("pre-install failure left restart record: %v", err)
	}

	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 {
		t.Fatalf("recovered publication bind calls=%d, want 1", bindCalls)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
}

func TestWakeSelfUpgradeRefusalWriteFailureNeverReexecutes(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartPending
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)

	bindCalls := 0
	previousBind := wakeRestartBind
	previousSync := syncWakeOwnerDirFD
	failNextSync := false
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		failNextSync = true
		return nil, errors.New("test bind refusal")
	}
	syncWakeOwnerDirFD = func(fd int) error {
		if failNextSync {
			failNextSync = false
			return errors.New("test refusal sync failure")
		}
		return previousSync(fd)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		syncWakeOwnerDirFD = previousSync
	})

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
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending == nil {
		t.Fatalf("first refusal = binds=%d debt=%#v", bindCalls, cfg.selfUpgrade.refusalPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefusalPending)

	failNextSync = true
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending == nil {
		t.Fatalf("failed refusal retry = binds=%d debt=%#v", bindCalls, cfg.selfUpgrade.refusalPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)

	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending != nil || cfg.selfUpgrade.restartPending {
		t.Fatalf(
			"completed refusal retry = binds=%d debt=%#v pending=%v",
			bindCalls,
			cfg.selfUpgrade.refusalPending,
			cfg.selfUpgrade.restartPending,
		)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefused)
}

func TestWakeSelfUpgradePostRenameRefusalErrorObservesInstalledRefusal(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartPending
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)

	bindCalls := 0
	previousBind := wakeRestartBind
	previousSync := syncWakeOwnerDirFD
	armed := false
	syncCalls := 0
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		armed = true
		syncCalls = 0
		return nil, errors.New("test bind refusal")
	}
	syncWakeOwnerDirFD = func(fd int) error {
		if armed {
			syncCalls++
			if syncCalls == 2 {
				armed = false
				return errors.New("test post-rename sync failure")
			}
		}
		return previousSync(fd)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		syncWakeOwnerDirFD = previousSync
	})

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
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending == nil {
		t.Fatalf("ambiguous refusal = binds=%d debt=%#v", bindCalls, cfg.selfUpgrade.refusalPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)

	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending != nil || cfg.selfUpgrade.restartPending {
		t.Fatalf(
			"observed refusal = binds=%d debt=%#v pending=%v",
			bindCalls,
			cfg.selfUpgrade.refusalPending,
			cfg.selfUpgrade.restartPending,
		)
	}
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefused)
}

func TestWakeSelfUpgradeRefusalRetryPostRenameErrorObservesInstalledRefusal(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartPending
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)

	bindCalls := 0
	previousBind := wakeRestartBind
	previousSync := syncWakeOwnerDirFD
	mode := ""
	syncCalls := 0
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		mode = "pre-install"
		return nil, errors.New("test bind refusal")
	}
	syncWakeOwnerDirFD = func(fd int) error {
		switch mode {
		case "pre-install":
			mode = ""
			return errors.New("test pre-install sync failure")
		case "post-rename":
			syncCalls++
			if syncCalls == 2 {
				mode = ""
				return errors.New("test post-rename sync failure")
			}
		}
		return previousSync(fd)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		syncWakeOwnerDirFD = previousSync
	})

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
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending == nil {
		t.Fatalf("initial refusal = binds=%d debt=%#v", bindCalls, cfg.selfUpgrade.refusalPending)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)

	mode = "post-rename"
	syncCalls = 0
	injectedPostRename := false
	previousSyncWithInjection := syncWakeOwnerDirFD
	syncWakeOwnerDirFD = func(fd int) error {
		err := previousSyncWithInjection(fd)
		if err != nil && strings.Contains(err.Error(), "post-rename") {
			injectedPostRename = true
		}
		return err
	}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if !injectedPostRename {
		t.Fatal("refusal retry did not inject the post-rename failure")
	}
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending != nil || cfg.selfUpgrade.restartPending {
		t.Fatalf(
			"post-rename retry = binds=%d debt=%#v pending=%v",
			bindCalls,
			cfg.selfUpgrade.refusalPending,
			cfg.selfUpgrade.restartPending,
		)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefused)
}

func TestWakeSelfUpgradeChangedRecordConsumesDebtThenHandlesReplacement(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	debtRecord := fixture.record
	debtRecord.Source = wakeRestartSourceSelf

	replacement := fixture.record
	replacement.Source = wakeRestartSourceForeign
	replacement.RequestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, replacement)

	bindCalls := 0
	previousBind := wakeRestartBind
	wakeRestartBind = func(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
		if record.RequestID == debtRecord.RequestID {
			t.Fatal("retired refusal debt was executed")
		}
		if record.RequestID != replacement.RequestID {
			t.Fatalf("unexpected replacement request %q", record.RequestID)
		}
		bindCalls++
		return nil, errors.New("test replacement bind refusal")
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
			refusalPending: &wakeSelfUpgradeRefusalPending{
				Record: debtRecord,
				Reason: "retired self-upgrade attempt",
			},
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending != nil {
		t.Fatalf("replacement handling = binds=%d debt=%#v", bindCalls, cfg.selfUpgrade.refusalPending)
	}
	installed := readWakeSelfUpgradeRestartRecord(t, fixture)
	if installed.RequestID != replacement.RequestID ||
		installed.Source != wakeRestartSourceForeign || installed.Status != wakeRestartRefused {
		t.Fatalf("replacement record=%#v", installed)
	}
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
