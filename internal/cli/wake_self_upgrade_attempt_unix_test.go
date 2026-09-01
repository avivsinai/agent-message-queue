//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

func TestWakeSelfUpgradeAttemptSurvivesStaleLockReclaimAndRefusesOldWake(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidatePath := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	candidate, err := captureWakeImageEvidence(candidatePath, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Candidate = candidate
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_001, 0)
	setWakeSelfUpgradeAttemptClock(t, now)
	if _, err := persistWakeSelfUpgradeAttemptAtBoundary(fixture.agentDir, fixture.lock, record); err != nil {
		t.Fatalf("persist replacement attempt: %v", err)
	}
	if got := readWakeSelfUpgradeAttemptForTest(t, fixture); got.Status != selfupgrade.AttemptStatusAttempt ||
		!got.Matches(candidate) {
		t.Fatalf("persisted attempt = %#v, want unsettled candidate B", got)
	}

	staleLock := fixture.lock.Lock
	staleLock.PID = 66121
	staleLock.ProcessStart = "stale-process"
	staleLock.BootID = "stale-boot"
	writeWakeLockForTest(t, fixture.root, fixture.agent, staleLock)
	staleInspection := inspectWakeLock(fixture.root, fixture.agent)
	if staleInspection.Status != wakeLockStale {
		t.Fatalf("stale lock inspection = %#v, want stale", staleInspection)
	}

	newAgentDir, newCleanup, err := acquireWakeLockWithOptionsRetained(
		fixture.root,
		fixture.agent,
		wakeLockAcquireOptions{
			wakeMode:       wakeInjectModeNone,
			resumeEligible: true,
			requestedOwner: &fixture.owner,
		},
	)
	if err != nil {
		t.Fatalf("new old-image wake reclaiming stale lock: %v", err)
	}
	defer func() {
		newCleanup()
		_ = newAgentDir.Close()
	}()
	if got := readWakeSelfUpgradeAttemptForTest(t, fixture); got.Status != selfupgrade.AttemptStatusAttempt ||
		!got.Matches(candidate) {
		t.Fatalf("attempt after stale-lock reclaim = %#v, want preserved candidate B", got)
	}
	newInspection := inspectWakeLock(fixture.root, fixture.agent)
	if !newInspection.IdentityConfirmed || newInspection.Status != wakeLockValid {
		t.Fatalf("new wake inspection = %#v, want valid identity-confirmed wake", newInspection)
	}
	if newInspection.Lock.RunningImageEvidence == nil ||
		!sameWakeImageEvidence(*newInspection.Lock.RunningImageEvidence, fixture.candidate) {
		t.Fatalf("new wake running image = %#v, want old image A", newInspection.Lock.RunningImageEvidence)
	}

	state := selfUpgradeStateForCandidate(t, candidatePath)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	if err := loadWakeSelfUpgradeAttemptAtStartup(
		&state,
		newAgentDir,
		newInspection,
		*newInspection.Lock.RunningImageEvidence,
	); err != nil {
		t.Fatalf("load preserved attempt in new wake: %v", err)
	}
	if len(state.attempts) != 1 || state.attempts[0].Status != selfupgrade.AttemptStatusAttempt ||
		!state.attempts[0].Matches(candidate) {
		t.Fatalf("new wake attempt state = %#v, want candidate B", state.attempts)
	}

	newInboxDir, err := openWakeRepairInboxDir(newAgentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newInboxDir.Close() }()
	execCalls := 0
	previousExec := wakeRestartExec
	wakeRestartExec = func(string, []string, []string) error {
		execCalls++
		return errors.New("unexpected exec")
	}
	t.Cleanup(func() { wakeRestartExec = previousExec })
	cfg := wakeConfig{
		me:                   fixture.agent,
		root:                 fixture.root,
		injectMode:           wakeInjectModeNone,
		wakeOwner:            newInspection.Lock.ResumeOwner,
		terminalGeneration:   newInspection.Lock.Generation,
		terminalImageVersion: newInspection.Lock.ImageVersion,
		retainedAgent:        newAgentDir,
		retainedInbox:        newInboxDir,
		selfUpgrade:          state,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		newAgentDir,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	); err != nil {
		t.Fatalf("maintenance after stale-lock reclaim: %v", err)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want zero after attempt refusal", execCalls)
	}
	if !wakeSelfUpgradeRefusedCandidatesContain(cfg.selfUpgrade.refused, candidate) {
		t.Fatalf("refusal memory = %#v, want candidate B", cfg.selfUpgrade.refused)
	}
	if got := readWakeSelfUpgradeAttemptForTest(t, fixture); got.Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("attempt after maintenance = %#v, want unsettled diagnostic record", got)
	}
	if _, err := os.Lstat(filepath.Join(newAgentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("maintenance created restart record after refusal: %v", err)
	}
}

func TestWakeSelfUpgradeAttemptPersistsBeforeRestartExec(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_000, 0))

	attempts, err := persistWakeSelfUpgradeAttemptAtBoundary(fixture.agentDir, fixture.lock, record)
	if err != nil {
		t.Fatalf("persistWakeSelfUpgradeAttemptAtBoundary() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != selfupgrade.AttemptStatusAttempt || !attempts[0].Matches(record.Candidate) {
		t.Fatalf("attempts = %#v, want current candidate attempt", attempts)
	}
	installed := readWakeSelfUpgradeAttemptForTest(t, fixture)
	if installed != attempts[0] {
		t.Fatalf("installed attempt = %#v, want %#v", installed, attempts[0])
	}
	info, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWakeSelfUpgradeSettledCandidateDoesNotRearmAttempt(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	setWakeSelfUpgradeAttemptClock(t, now)
	settled := selfupgrade.NewAttempt(record.Candidate, now.Add(-time.Second))
	settled.Status = selfupgrade.AttemptStatusSettled
	writeWakeSelfUpgradeAttemptForTest(t, fixture, settled)
	before, err := os.ReadFile(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName))
	if err != nil {
		t.Fatal(err)
	}

	attempts, err := persistWakeSelfUpgradeAttemptAtBoundary(fixture.agentDir, fixture.lock, record)
	if err != nil {
		t.Fatalf("persistWakeSelfUpgradeAttemptAtBoundary() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0] != settled {
		t.Fatalf("attempts = %#v, want unchanged settled entry", attempts)
	}
	after, err := os.ReadFile(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("settled wake attempt changed during classification:\nbefore=%safter=%s", before, after)
	}
}

func TestWakeSelfUpgradeAttemptBoundaryPreservesFutureUncertainEntry(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	setWakeSelfUpgradeAttemptClock(t, now)
	future := selfupgrade.NewAttempt(record.Candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, future)

	if _, err := persistWakeSelfUpgradeAttemptAtBoundary(fixture.agentDir, fixture.lock, record); !errors.Is(err, errWakeSelfUpgradeAttemptTimestampUncertain) {
		t.Fatalf("persistWakeSelfUpgradeAttemptAtBoundary() error = %v, want timestamp uncertainty", err)
	}
	if got := readWakeSelfUpgradeAttemptForTest(t, fixture); got != future {
		t.Fatalf("future attempt = %#v, want preserved entry", got)
	}
}

func TestWakeSelfUpgradeSettlementPreservesFutureUncertainEntry(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	now := time.Unix(1_700_000_000, 0).UTC()
	setWakeSelfUpgradeAttemptClock(t, now)
	future := selfupgrade.NewAttempt(fixture.candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, future)
	state := wakeSelfUpgradeState{
		Enabled:  true,
		Eligible: true,
		attempts: []selfupgrade.Attempt{future},
	}

	if err := settleWakeSelfUpgradeAttemptAtBoundary(&state, fixture.agentDir, fixture.lock, fixture.candidate); err != nil {
		t.Fatalf("settleWakeSelfUpgradeAttemptAtBoundary() error = %v", err)
	}
	if state.Eligible || state.Reason != "self-upgrade unavailable: replacement attempt timestamp is uncertain" {
		t.Fatalf("state = %#v, want unavailable", state)
	}
	if got := readWakeSelfUpgradeAttemptForTest(t, fixture); got != future {
		t.Fatalf("future attempt after settlement = %#v, want preserved entry", got)
	}
}

func TestWakeSelfUpgradeAttemptSettlesAtFirstQuiescentBoundary(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidate := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(candidate, locator); err != nil {
		t.Fatal(err)
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_000, 0))
	attempt := selfupgrade.NewAttempt(fixture.candidate, time.Unix(1_700_000_000, 0))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, attempt)
	state := wakeSelfUpgradeState{
		Enabled:   true,
		Eligible:  true,
		Locator:   locator,
		lastProbe: probe,
		attempts:  []selfupgrade.Attempt{attempt},
	}
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
	}

	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	); err != nil {
		t.Fatalf("maintainWakeSelfUpgradeAtLoopBoundary() error = %v", err)
	}
	installed := readWakeSelfUpgradeAttemptForTest(t, fixture)
	if installed.Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("installed attempt status = %q, want settled", installed.Status)
	}
	if len(cfg.selfUpgrade.attempts) != 1 || cfg.selfUpgrade.attempts[0].Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("state attempts = %#v, want one settled attempt", cfg.selfUpgrade.attempts)
	}
}

func TestLoadWakeSelfUpgradeUnsettledAttemptRefusesMatchingImage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_001, 0))
	attempt := selfupgrade.NewAttempt(fixture.candidate, time.Unix(1_700_000_000, 0))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, attempt)
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true}

	if err := loadWakeSelfUpgradeAttemptAtStartup(&state, fixture.agentDir, fixture.lock, fixture.candidate); err != nil {
		t.Fatalf("loadWakeSelfUpgradeAttemptAtStartup() error = %v", err)
	}
	if len(state.attempts) != 1 || state.attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("loaded attempts = %#v, want one unsettled attempt", state.attempts)
	}
	if len(state.refused) != 0 {
		t.Fatalf("startup refusal memory = %#v, want diagnostic-only load", state.refused)
	}
	if state.startupRefusalReason == "" {
		t.Fatal("startup refusal reason is empty")
	}
	if state.lastProbe != (wakeSelfUpgradeProbe{}) {
		t.Fatalf("last probe = %#v, want unchanged diagnostic-only state", state.lastProbe)
	}
}

func TestLoadWakeSelfUpgradeSettledAttemptDoesNotRefuseMatchingImage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_001, 0))
	attempt := selfupgrade.NewAttempt(fixture.candidate, time.Unix(1_700_000_000, 0))
	attempt.Status = selfupgrade.AttemptStatusSettled
	writeWakeSelfUpgradeAttemptForTest(t, fixture, attempt)
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true}

	if err := loadWakeSelfUpgradeAttemptAtStartup(&state, fixture.agentDir, fixture.lock, fixture.candidate); err != nil {
		t.Fatalf("loadWakeSelfUpgradeAttemptAtStartup() error = %v", err)
	}
	if len(state.attempts) != 1 || state.attempts[0].Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("loaded attempts = %#v, want one settled attempt", state.attempts)
	}
	if len(state.refused) != 0 || state.startupRefusalReason != "" {
		t.Fatalf("settled startup state = %#v, want no refusal", state)
	}
}

func TestLoadWakeSelfUpgradeSchema1AttemptAsLedger(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	now := time.Unix(1_700_000_001, 0)
	setWakeSelfUpgradeAttemptClock(t, now)
	attempt := selfupgrade.NewAttempt(fixture.candidate, now.Add(-time.Second))
	legacy := wakeSelfUpgradeAttemptFile{
		Schema:    wakeSelfUpgradeAttemptSchemaV1,
		Status:    attempt.Status,
		Candidate: &attempt.Candidate,
		UnixTime:  attempt.UnixTime,
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true}
	if err := loadWakeSelfUpgradeAttemptAtStartup(&state, fixture.agentDir, fixture.lock, fixture.candidate); err != nil {
		t.Fatalf("load schema 1 attempt: %v", err)
	}
	if len(state.attempts) != 1 || state.attempts[0] != attempt {
		t.Fatalf("loaded attempts = %#v, want %#v", state.attempts, attempt)
	}
}

func TestLoadWakeSelfUpgradeFutureAttemptDisablesEligibility(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	now := time.Unix(1_700_000_001, 0)
	setWakeSelfUpgradeAttemptClock(t, now)
	attempt := selfupgrade.NewAttempt(fixture.candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, attempt)
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true}
	if err := loadWakeSelfUpgradeAttemptAtStartup(&state, fixture.agentDir, fixture.lock, fixture.candidate); err != nil {
		t.Fatalf("load future attempt: %v", err)
	}
	if state.Eligible || state.Reason != "self-upgrade unavailable: replacement attempt timestamp is uncertain" {
		t.Fatalf("future attempt state = %#v, want unavailable", state)
	}
	if len(state.attempts) != 1 || state.attempts[0] != attempt {
		t.Fatalf("future attempt ledger = %#v, want preserved record", state.attempts)
	}
}

func TestWakeSelfUpgradeMaintenanceHonorsRefusalMemory(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidate := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	evidence, err := captureWakeImageEvidence(candidate, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	state.refused = rememberWakeSelfUpgradeRefusal(nil, evidence)

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatalf("maintainWakeSelfUpgrade() error = %v", err)
	}
	if decision.Action != wakeSelfUpgradeActionRefusedMemory || decision.Reason != "candidate was already refused for this wake generation" {
		t.Fatalf("decision = %#v, want refusal memory", decision)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("startup refusal created restart record: %v", err)
	}
}

func TestWakeSelfUpgradeMaintenanceRefusesUnsettledAttemptMatchingCandidate(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidate := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	evidence, err := captureWakeImageEvidence(candidate, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_001, 0))
	attempt := selfupgrade.NewAttempt(evidence, time.Unix(1_700_000_000, 0))
	state.attempts = []selfupgrade.Attempt{attempt}

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatalf("maintainWakeSelfUpgrade() error = %v", err)
	}
	if decision.Action != wakeSelfUpgradeActionRefusedMemory || decision.Reason != attempt.RefusalReason() {
		t.Fatalf("decision = %#v, want unsettled-attempt refusal", decision)
	}
	if !wakeSelfUpgradeRefusedCandidatesContain(state.refused, evidence) {
		t.Fatalf("refusal memory = %#v, want candidate", state.refused)
	}
	if len(state.attempts) != 1 || state.attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("attempt state = %#v, want one unsettled attempt", state.attempts)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("unsettled-attempt refusal created restart record: %v", err)
	}
}

func TestWakeSelfUpgradeAttemptDoesNotSettleDifferentRunningImage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	candidate := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(candidate, locator); err != nil {
		t.Fatal(err)
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_000, 0))
	evidence, err := captureWakeImageEvidence(candidate, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	attempt := selfupgrade.NewAttempt(evidence, time.Unix(1_700_000_000, 0))
	writeWakeSelfUpgradeAttemptForTest(t, fixture, attempt)
	state := wakeSelfUpgradeState{
		Enabled:   true,
		Eligible:  true,
		Locator:   locator,
		lastProbe: probe,
		attempts:  []selfupgrade.Attempt{attempt},
	}
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
	}

	if err := maintainWakeSelfUpgradeAtLoopBoundary(
		&cfg,
		fixture.agentDir,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	); err != nil {
		t.Fatalf("maintainWakeSelfUpgradeAtLoopBoundary() error = %v", err)
	}
	installed := readWakeSelfUpgradeAttemptForTest(t, fixture)
	if installed.Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("installed attempt status = %q, want unsettled", installed.Status)
	}
	if len(cfg.selfUpgrade.attempts) != 1 || cfg.selfUpgrade.attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("state attempts = %#v, want one unsettled attempt", cfg.selfUpgrade.attempts)
	}
}

func TestWakeSelfUpgradeAttemptCleanupAfterLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	evidence, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	attempt := selfupgrade.NewAttempt(evidence, time.Now())
	writeWakeSelfUpgradeAttemptForGenericCleanupTest(t, fixture, attempt)

	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return removeWakeLockIfUnchangedGuardedAt(scope, fixture.created)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName)); err != nil {
		t.Fatalf("self-upgrade attempt after generic lock removal = %v, want preserved", err)
	}
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeSelfUpgradeAttemptAt(dirfd)
	}); err != nil {
		t.Fatalf("explicit wake retire attempt removal: %v", err)
	}
	assertPathMissingForTest(t, filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName))
}

func setWakeSelfUpgradeAttemptClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := wakeSelfUpgradeNow
	wakeSelfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { wakeSelfUpgradeNow = previous })
}

func writeWakeSelfUpgradeAttemptForTest(t *testing.T, fixture wakeRestartFixture, attempt selfupgrade.Attempt) {
	t.Helper()
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir, []selfupgrade.Attempt{attempt})
	}); err != nil {
		t.Fatal(err)
	}
}

func readWakeSelfUpgradeAttemptForTest(t *testing.T, fixture wakeRestartFixture) selfupgrade.Attempt {
	t.Helper()
	var attempts []selfupgrade.Attempt
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var exists bool
		var err error
		attempts, exists, err = readWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return os.ErrNotExist
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt ledger = %#v, want one attempt", attempts)
	}
	return attempts[0]
}

func writeWakeSelfUpgradeAttemptForGenericCleanupTest(
	t *testing.T,
	fixture *genericWakePreparedCleanupFixture,
	attempt selfupgrade.Attempt,
) {
	t.Helper()
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir, []selfupgrade.Attempt{attempt})
	}); err != nil {
		t.Fatal(err)
	}
}
