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

func TestWakeSelfUpgradeRefusalMemoryBlocksABARetryWithinGeneration(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidateA := writeWakeSelfUpgradeCandidate(t, dir, "candidate-a")
	candidateB := writeWakeSelfUpgradeCandidate(t, dir, "candidate-b")
	state := selfUpgradeStateForCandidate(t, candidateA)
	stubWakeSelfUpgradeVersion(t, "9.9.9")
	evidenceA, err := captureWakeImageEvidence(candidateA, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	evidenceB, err := captureWakeImageEvidence(candidateB, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}

	binds := map[string]int{}
	preflights := 0
	notifies := 0
	execs := 0
	previousBind := wakeRestartBind
	previousBoundPreflight := wakeRestartBoundPreflight
	previousNotify := wakeRestartNotify
	previousExec := wakeRestartExec
	previousSync := syncWakeOwnerDirFD
	supersedingFailurePhase := ""
	wakeRestartBind = func(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
		switch {
		case sameWakeSelfUpgradeCandidateIdentity(record.Candidate, evidenceA):
			binds["a"]++
		case sameWakeSelfUpgradeCandidateIdentity(record.Candidate, evidenceB):
			binds["b"]++
		default:
			t.Fatalf("bound unexpected candidate: %#v", record.Candidate)
		}
		return nil, errors.New("test candidate refusal")
	}
	wakeRestartBoundPreflight = func(*wakeRestartBoundImage, []string, wakeResumeBootstrap) error {
		preflights++
		return errors.New("unexpected preflight")
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifies++
		return errors.New("unexpected notify")
	}
	wakeRestartExec = func(string, []string, []string) error {
		execs++
		return errors.New("unexpected exec")
	}
	syncWakeOwnerDirFD = func(fd int) error {
		if supersedingFailurePhase != "" {
			entries, err := os.ReadDir(fixture.agentDir.path)
			if err != nil {
				return err
			}
			tempPresent := false
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".wake.restart.tmp.") {
					tempPresent = true
					break
				}
			}
			if supersedingFailurePhase == "pre-install" && tempPresent {
				supersedingFailurePhase = ""
				return errors.New("test superseding pre-install failure")
			}
			if supersedingFailurePhase == "post-rename" && !tempPresent {
				raw, readErr := os.ReadFile(filepath.Join(fixture.agentDir.path, wakeRestartFileName))
				if readErr == nil && strings.Contains(string(raw), evidenceB.SHA256) {
					supersedingFailurePhase = ""
					return errors.New("test superseding post-rename failure")
				}
			}
		}
		return previousSync(fd)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		wakeRestartBoundPreflight = previousBoundPreflight
		wakeRestartNotify = previousNotify
		wakeRestartExec = previousExec
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
	tick := func() {
		t.Helper()
		if err := maintainWakeSelfUpgradeAtLoopBoundary(
			&cfg,
			fixture.agentDir,
			watcher,
			false,
			false,
		); err != nil {
			t.Fatal(err)
		}
	}
	quarantineCount := func() int {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(
			fixture.agentDir.path,
			wakeRestartFileName+".quarantined.*",
		))
		if err != nil {
			t.Fatal(err)
		}
		return len(paths)
	}

	tick()
	if binds["a"] != 1 || binds["b"] != 0 || preflights != 0 || notifies != 0 || execs != 0 {
		t.Fatalf("A attempt counters: bind=%v preflight=%d notify=%d exec=%d", binds, preflights, notifies, execs)
	}
	refusedA := readWakeSelfUpgradeRestartRecord(t, fixture)
	beforeSupersession := quarantineCount()
	replaceWakeSelfUpgradeLocator(t, cfg.selfUpgrade.Locator, candidateB)
	supersedingFailurePhase = "pre-install"
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err == nil || !strings.Contains(err.Error(), "test superseding pre-install failure") {
		t.Fatalf("superseding publication failure = %v", err)
	}
	if supersedingFailurePhase != "" {
		t.Fatal("superseding publication failure was not injected")
	}
	if binds["a"] != 1 || binds["b"] != 0 || quarantineCount() != beforeSupersession {
		t.Fatalf("failed B publication changed execution or quarantine: bind=%v quarantine=%d", binds, quarantineCount())
	}
	if got := readWakeSelfUpgradeRestartRecord(t, fixture); !sameWakeRestartRecord(got, refusedA) {
		t.Fatalf("failed B publication lost A refusal: got=%#v want=%#v", got, refusedA)
	}

	supersedingFailurePhase = "post-rename"
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		watcher,
		false,
		false,
	); err == nil || !strings.Contains(err.Error(), "test superseding post-rename failure") {
		t.Fatalf("superseding post-rename failure = %v", err)
	}
	if supersedingFailurePhase != "" {
		t.Fatal("superseding post-rename failure was not injected")
	}
	freshAgentDir, err := openWakeAgentDir(fixture.root, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = freshAgentDir.Close() }()
	var persistedB wakeRestartRecord
	if err := freshAgentDir.withFD(func(dirfd int) error {
		var exists bool
		var readErr error
		persistedB, exists, readErr = readWakeRestartRecordAt(dirfd, freshAgentDir)
		if readErr != nil {
			return readErr
		}
		if !exists {
			return os.ErrNotExist
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if persistedB.Status != wakeRestartPending ||
		!sameWakeSelfUpgradeCandidateIdentity(persistedB.Candidate, evidenceB) ||
		len(persistedB.RefusedCandidates) != 1 ||
		!wakeSelfUpgradeRefusedCandidatesContain(persistedB.RefusedCandidates, evidenceA) {
		t.Fatalf("fresh read after B post-rename failure = %#v", persistedB)
	}
	if quarantineCount() != beforeSupersession {
		t.Fatalf("post-rename B publication grew quarantine: before=%d after=%d", beforeSupersession, quarantineCount())
	}

	tick()
	if binds["a"] != 1 || binds["b"] != 1 || preflights != 0 || notifies != 0 || execs != 0 {
		t.Fatalf("B attempt counters: bind=%v preflight=%d notify=%d exec=%d", binds, preflights, notifies, execs)
	}
	beforeRememberedA := quarantineCount()
	if beforeRememberedA != beforeSupersession {
		t.Fatalf("same-scope A to B supersession grew quarantine: before=%d after=%d", beforeSupersession, beforeRememberedA)
	}
	refusedB := readWakeSelfUpgradeRestartRecord(t, fixture)
	if refusedB.Status != wakeRestartRefused ||
		!sameWakeSelfUpgradeCandidateIdentity(refusedB.Candidate, evidenceB) ||
		len(refusedB.RefusedCandidates) != 2 ||
		!wakeSelfUpgradeRefusedCandidatesContain(refusedB.RefusedCandidates, evidenceA) ||
		!wakeSelfUpgradeRefusedCandidatesContain(refusedB.RefusedCandidates, evidenceB) {
		t.Fatalf("B refusal memory = %#v", refusedB)
	}

	replaceWakeSelfUpgradeLocator(t, cfg.selfUpgrade.Locator, candidateA)
	tick()
	if binds["a"] != 1 || binds["b"] != 1 || preflights != 0 || notifies != 0 || execs != 0 {
		t.Fatalf("remembered A was executed: bind=%v preflight=%d notify=%d exec=%d", binds, preflights, notifies, execs)
	}
	if got := quarantineCount(); got != beforeRememberedA {
		t.Fatalf("remembered A grew quarantine: before=%d after=%d", beforeRememberedA, got)
	}
	if got := readWakeSelfUpgradeRestartRecord(t, fixture); !sameWakeRestartRecord(got, refusedB) {
		t.Fatalf("remembered A churned active refusal: got=%#v want=%#v", got, refusedB)
	}
	tick()
	if binds["a"] != 1 || binds["b"] != 1 || quarantineCount() != beforeRememberedA {
		t.Fatalf("unchanged remembered A churned state: bind=%v quarantine=%d", binds, quarantineCount())
	}
}

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
	tests := []struct {
		name          string
		changeLocator func(*testing.T, string)
		deferOnce     bool
	}{
		{
			name: "locator replaced",
			changeLocator: func(t *testing.T, locator string) {
				replacement := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "replacement")
				replaceWakeSelfUpgradeLocator(t, locator, replacement)
			},
		},
		{
			name: "locator missing across quiescence deferral",
			changeLocator: func(t *testing.T, locator string) {
				if err := os.Remove(locator); err != nil {
					t.Fatal(err)
				}
			},
			deferOnce: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			removeWakeRestartRecordForTest(t, fixture)
			candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
			state := selfUpgradeStateForCandidate(t, candidate)
			stubWakeSelfUpgradeVersion(t, "0.57.0")

			bindCalls := 0
			var boundRecord wakeRestartRecord
			previousBind := wakeRestartBind
			wakeRestartBind = func(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
				bindCalls++
				boundRecord = record
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
				t.Fatalf(
					"failed publication handled early: binds=%d pending=%v",
					bindCalls,
					cfg.selfUpgrade.restartPending,
				)
			}
			assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)
			published := readWakeSelfUpgradeRestartRecord(t, fixture)
			test.changeLocator(t, cfg.selfUpgrade.Locator)
			wakeSelfUpgradeReadPublished = previousReadPublished

			if test.deferOnce {
				cfg.inputDelivery = wakeInputDeliveryState{
					phase:         wakeInputPrimarySubmitPending,
					acceptedBytes: 1,
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
				if bindCalls != 0 || !cfg.selfUpgrade.restartPending {
					t.Fatalf(
						"deferred adoption = binds=%d pending=%v",
						bindCalls,
						cfg.selfUpgrade.restartPending,
					)
				}
				assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)
				cfg.inputDelivery.reset()
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
			if bindCalls != 1 || cfg.selfUpgrade.restartPending || cfg.selfUpgrade.refusalPending != nil {
				t.Fatalf(
					"adopted publication = binds=%d pending=%v refusal=%#v",
					bindCalls,
					cfg.selfUpgrade.restartPending,
					cfg.selfUpgrade.refusalPending,
				)
			}
			if !sameWakeSelfUpgradeCandidateIdentity(boundRecord.Candidate, published.Candidate) {
				t.Fatalf(
					"bound candidate=%#v, want published candidate %#v",
					boundRecord.Candidate,
					published.Candidate,
				)
			}
			assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
		})
	}
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

func TestPendingWakeSelfUpgradeForProcessAdoptsOnlyExactAuthority(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *wakeRestartRecord)
		inspect    func(wakeRestartFixture) wakeLockInspection
		noOwner    bool
		wantAdopt  bool
		wantErrSub string
	}{
		{name: "exact self pending", wantAdopt: true},
		{
			name: "foreign pending",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Source = wakeRestartSourceForeign
			},
		},
		{
			name: "claimed successor",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Schema = wakeRestartSchemaV2
				record.SuccessorGeneration = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "refused",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Status = wakeRestartRefused
				record.Reason = "test refusal"
			},
		},
		{
			name: "different root",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Root += "-other"
			},
		},
		{
			name: "different agent",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Agent = "claude"
			},
		},
		{
			name: "different generation",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "different owner",
			mutate: func(_ *testing.T, record *wakeRestartRecord) {
				record.Owner.SessionID++
			},
		},
		{
			name: "different incumbent pid",
			inspect: func(fixture wakeRestartFixture) wakeLockInspection {
				lock := fixture.lock
				lock.PID++
				lock.Lock.PID = lock.PID
				return lock
			},
			wantErrSub: "incumbent changed",
		},
		{
			name: "different incumbent generation",
			inspect: func(fixture wakeRestartFixture) wakeLockInspection {
				lock := fixture.lock
				lock.Lock.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				return lock
			},
			wantErrSub: "incumbent changed",
		},
		{
			name: "different incumbent resume owner",
			inspect: func(fixture wakeRestartFixture) wakeLockInspection {
				lock := fixture.lock
				owner := fixture.owner
				owner.SessionID++
				lock.Lock.ResumeOwner = &owner
				return lock
			},
			wantErrSub: "incumbent changed",
		},
		{
			name: "missing incumbent resume owner",
			inspect: func(fixture wakeRestartFixture) wakeLockInspection {
				lock := fixture.lock
				lock.Lock.ResumeOwner = nil
				return lock
			},
			wantErrSub: "incumbent changed",
		},
		{
			name:    "owner unavailable",
			noOwner: true,
		},
		{
			name: "unverified incumbent",
			inspect: func(fixture wakeRestartFixture) wakeLockInspection {
				return wakeLockInspection{
					Exists: true,
					Status: wakeLockUnverified,
					Reason: "test transient lock read failure",
					Root:   fixture.root,
					Agent:  fixture.agent,
				}
			},
			wantErrSub: "incumbent changed",
		},
	}
	if runtime.GOOS == "darwin" {
		tests = append(tests, struct {
			name       string
			mutate     func(*testing.T, *wakeRestartRecord)
			inspect    func(wakeRestartFixture) wakeLockInspection
			noOwner    bool
			wantAdopt  bool
			wantErrSub string
		}{
			name: "different previous bound image",
			mutate: func(t *testing.T, record *wakeRestartRecord) {
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
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			record := fixture.record
			record.Source = wakeRestartSourceSelf
			record.Status = wakeRestartPending
			if test.mutate != nil {
				test.mutate(t, &record)
			}
			writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)
			recordPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
			before, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}

			previousInspectLockAt := wakeSelfUpgradeInspectLockAt
			if test.inspect != nil {
				wakeSelfUpgradeInspectLockAt = func(
					int,
					*wakeAgentDir,
					string,
					string,
				) wakeLockInspection {
					return test.inspect(fixture)
				}
			}
			t.Cleanup(func() { wakeSelfUpgradeInspectLockAt = previousInspectLockAt })

			owner := &fixture.owner
			if test.noOwner {
				owner = nil
			}
			cfg := wakeConfig{
				me:                 fixture.agent,
				root:               fixture.root,
				wakeOwner:          owner,
				terminalGeneration: fixture.lock.Lock.Generation,
			}
			adoptedRecord, adopted, err := pendingWakeSelfUpgradeForProcess(&cfg, fixture.agentDir)
			if test.wantErrSub == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrSub)) {
				t.Fatalf("error=%v, want substring %q", err, test.wantErrSub)
			}
			if adopted != test.wantAdopt {
				t.Fatalf("adopted=%v, want %v", adopted, test.wantAdopt)
			}
			if adopted && !sameWakeRestartAttemptIdentity(record, adoptedRecord) {
				t.Fatalf("adopted record=%#v, want %#v", adoptedRecord, record)
			}
			after, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("authority reconciliation mutated the restart record")
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
	previousInspectLockAt := wakeSelfUpgradeInspectLockAt
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
		wakeSelfUpgradeInspectLockAt = previousInspectLockAt
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

	transientLockRead := true
	wakeSelfUpgradeInspectLockAt = func(
		dirfd int,
		agentDir *wakeAgentDir,
		root, me string,
	) wakeLockInspection {
		if transientLockRead {
			transientLockRead = false
			return wakeLockInspection{
				Exists:         true,
				Status:         wakeLockUnverified,
				Reason:         "test transient lock read failure",
				observationErr: errors.New("test transient lock read failure"),
			}
		}
		return previousInspectLockAt(dirfd, agentDir, root, me)
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
	if bindCalls != 1 || cfg.selfUpgrade.refusalPending == nil || !cfg.selfUpgrade.restartPending {
		t.Fatalf(
			"inconclusive lock retry = binds=%d debt=%#v pending=%v",
			bindCalls,
			cfg.selfUpgrade.refusalPending,
			cfg.selfUpgrade.restartPending,
		)
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

func TestWakeSelfUpgradeAuthorityLossConsumesDebtWithoutReexecution(t *testing.T) {
	tests := []struct {
		name       string
		inspection func(wakeRestartFixture) wakeLockInspection
	}{
		{
			name: "changed resume owner",
			inspection: func(fixture wakeRestartFixture) wakeLockInspection {
				lock := fixture.lock
				changedOwner := fixture.owner
				changedOwner.SessionID++
				lock.Lock.ResumeOwner = &changedOwner
				lock.IdentityConfirmed = true
				return lock
			},
		},
		{
			name: "stale lock",
			inspection: func(fixture wakeRestartFixture) wakeLockInspection {
				return wakeLockInspection{
					Exists: true,
					Status: wakeLockStale,
					Reason: "pid not running",
					Root:   fixture.root,
					Agent:  fixture.agent,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			record := fixture.record
			record.Source = wakeRestartSourceSelf
			record.Status = wakeRestartPending
			writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)

			bindCalls := 0
			previousBind := wakeRestartBind
			wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
				bindCalls++
				return nil, errors.New("old refusal debt was re-executed")
			}
			previousInspectLockAt := wakeSelfUpgradeInspectLockAt
			wakeSelfUpgradeInspectLockAt = func(
				int,
				*wakeAgentDir,
				string,
				string,
			) wakeLockInspection {
				return test.inspection(fixture)
			}
			t.Cleanup(func() {
				wakeRestartBind = previousBind
				wakeSelfUpgradeInspectLockAt = previousInspectLockAt
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
					refusalPending: &wakeSelfUpgradeRefusalPending{
						Record: record,
						Reason: "test refusal debt",
					},
				},
				inspectTerminalGeneration: func() wakeLockInspection {
					return inspectWakeLock(fixture.root, fixture.agent)
				},
				restartSignals: make(chan os.Signal, 1),
			}
			handleWakeRestartAtLoopBoundary(
				&cfg,
				fixedWakeAdmissionWatcher{errors: make(chan error)},
				false,
				false,
			)
			if bindCalls != 0 || cfg.selfUpgrade.refusalPending != nil || cfg.selfUpgrade.restartPending {
				t.Fatalf(
					"authority loss = binds=%d debt=%#v pending=%v",
					bindCalls,
					cfg.selfUpgrade.refusalPending,
					cfg.selfUpgrade.restartPending,
				)
			}
			assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartPending)
		})
	}
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

func TestWakeSelfUpgradeChangedRecordDoesNotExecuteClaimedSuccessor(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	debtRecord := fixture.record
	debtRecord.Source = wakeRestartSourceSelf

	replacement := debtRecord
	replacement.RequestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	replacement.Schema = wakeRestartSchemaV2
	replacement.SuccessorGeneration = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, replacement)
	recordPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}

	bindCalls := 0
	previousBind := wakeRestartBind
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		return nil, errors.New("claimed successor was re-executed")
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
	handleWakeRestartAtLoopBoundary(
		&cfg,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	)
	if bindCalls != 0 || cfg.selfUpgrade.refusalPending != nil || cfg.selfUpgrade.restartPending {
		t.Fatalf(
			"claimed successor handling = binds=%d debt=%#v pending=%v",
			bindCalls,
			cfg.selfUpgrade.refusalPending,
			cfg.selfUpgrade.restartPending,
		)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("claimed successor record was mutated")
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
