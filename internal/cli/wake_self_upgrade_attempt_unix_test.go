//go:build darwin || linux

package cli

import (
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

func setWakeSelfUpgradeAttemptClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := wakeSelfUpgradeNow
	wakeSelfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { wakeSelfUpgradeNow = previous })
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
