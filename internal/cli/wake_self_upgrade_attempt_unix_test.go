//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

func TestWakeSelfUpgradeAttemptPersistsBeforeRestartExec(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	setWakeSelfUpgradeAttemptClock(t, time.Unix(1_700_000_000, 0))

	attempt, err := persistWakeSelfUpgradeAttemptAtBoundary(fixture.agentDir, fixture.lock, record)
	if err != nil {
		t.Fatalf("persistWakeSelfUpgradeAttemptAtBoundary() error = %v", err)
	}
	if attempt.Status != selfupgrade.AttemptStatusAttempt || !attempt.Matches(record.Candidate) {
		t.Fatalf("attempt = %#v, want current candidate attempt", attempt)
	}
	installed := readWakeSelfUpgradeAttemptForTest(t, fixture)
	if installed != attempt {
		t.Fatalf("installed attempt = %#v, want %#v", installed, attempt)
	}
	info, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeAttemptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt mode = %o, want 600", info.Mode().Perm())
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
		attempt:   &attempt,
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
	if cfg.selfUpgrade.attempt == nil || cfg.selfUpgrade.attempt.Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("state attempt = %#v, want settled", cfg.selfUpgrade.attempt)
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
	if state.attempt == nil || state.attempt.Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("loaded attempt = %#v, want unsettled attempt", state.attempt)
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
	if state.attempt == nil || state.attempt.Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("loaded attempt = %#v, want settled attempt", state.attempt)
	}
	if len(state.refused) != 0 || state.startupRefusalReason != "" {
		t.Fatalf("settled startup state = %#v, want no refusal", state)
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
	state.attempt = &attempt

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
	if state.attempt == nil || state.attempt.Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("attempt state = %#v, want unsettled attempt", state.attempt)
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
		attempt:   &attempt,
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
	if cfg.selfUpgrade.attempt == nil || cfg.selfUpgrade.attempt.Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("state attempt = %#v, want unsettled", cfg.selfUpgrade.attempt)
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

	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, fixture.created)
	}); err != nil {
		t.Fatal(err)
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
		return writeWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir, attempt)
	}); err != nil {
		t.Fatal(err)
	}
}

func readWakeSelfUpgradeAttemptForTest(t *testing.T, fixture wakeRestartFixture) selfupgrade.Attempt {
	t.Helper()
	var attempt selfupgrade.Attempt
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var exists bool
		var err error
		attempt, exists, err = readWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir)
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
	return attempt
}

func writeWakeSelfUpgradeAttemptForGenericCleanupTest(
	t *testing.T,
	fixture *genericWakePreparedCleanupFixture,
	attempt selfupgrade.Attempt,
) {
	t.Helper()
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeSelfUpgradeAttemptAt(dirfd, fixture.agentDir, attempt)
	}); err != nil {
		t.Fatal(err)
	}
}
