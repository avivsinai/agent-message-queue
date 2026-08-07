//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWakeSelfUpgradeCandidate(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func replaceWakeSelfUpgradeLocator(t *testing.T, locator, target string) {
	t.Helper()
	temporary := locator + ".next"
	if err := os.Symlink(target, temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, locator); err != nil {
		t.Fatal(err)
	}
}

func selfUpgradeStateForCandidate(t *testing.T, target string) wakeSelfUpgradeState {
	t.Helper()
	locator := filepath.Join(t.TempDir(), "amq")
	initial := writeWakeSelfUpgradeCandidate(t, filepath.Dir(locator), "initial")
	if err := os.Symlink(initial, locator); err != nil {
		t.Fatal(err)
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	replaceWakeSelfUpgradeLocator(t, locator, target)
	return wakeSelfUpgradeState{
		Enabled:   true,
		Eligible:  true,
		Locator:   locator,
		lastProbe: probe,
	}
}

func stubWakeSelfUpgradeVersion(t *testing.T, version string) {
	t.Helper()
	previous := wakeSelfUpgradeRunVersion
	wakeSelfUpgradeRunVersion = func(string) (string, error) { return version, nil }
	t.Cleanup(func() { wakeSelfUpgradeRunVersion = previous })
}

func TestMaintainWakeSelfUpgradePrefilterRejectsEqualAndDowngrade(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidateDir := t.TempDir()
	first := writeWakeSelfUpgradeCandidate(t, candidateDir, "equal")
	state := selfUpgradeStateForCandidate(t, first)

	stubWakeSelfUpgradeVersion(t, fixture.lock.Lock.ImageVersion)
	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionPrefilterRefused {
		t.Fatalf("equal-version decision = %#v", decision)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("equal-version probe created restart record: %v", err)
	}

	second := writeWakeSelfUpgradeCandidate(t, candidateDir, "downgrade")
	replaceWakeSelfUpgradeLocator(t, state.Locator, second)
	stubWakeSelfUpgradeVersion(t, "0.1.0")
	decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionPrefilterRefused {
		t.Fatalf("downgrade decision = %#v", decision)
	}
	if !strings.Contains(decision.Reason, "amq wake check") {
		t.Fatalf("downgrade remedy = %q", decision.Reason)
	}
}

func TestMaintainWakeSelfUpgradeMissingLocatorRetries(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	locator := filepath.Join(dir, "amq")
	missing := filepath.Join(dir, "missing")
	if err := os.Symlink(missing, locator); err != nil {
		t.Fatal(err)
	}
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true, Locator: locator}
	stubWakeSelfUpgradeVersion(t, fixture.lock.Lock.ImageVersion)

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionNoCandidate {
		t.Fatalf("missing locator decision = %#v", decision)
	}
	writeWakeSelfUpgradeCandidate(t, dir, "missing")
	decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionPrefilterRefused {
		t.Fatalf("recovered locator decision = %#v", decision)
	}
}

func TestCaptureWakeSelfUpgradeStartupStateDistinguishesPinnedAndSymlinkLocator(t *testing.T) {
	dir := t.TempDir()
	target := writeWakeSelfUpgradeCandidate(t, dir, "amq")
	running, err := captureWakeImageEvidence(target, "0.56.0")
	if err != nil {
		t.Fatal(err)
	}

	pinned := captureWakeSelfUpgradeStartupState(target, true, running)
	if pinned.Eligible || pinned.Reason == "" {
		t.Fatalf("direct image state = %#v", pinned)
	}
	locator := filepath.Join(dir, "stable-amq")
	if err := os.Symlink(target, locator); err != nil {
		t.Fatal(err)
	}
	stable := captureWakeSelfUpgradeStartupState(locator, true, running)
	if !stable.Eligible || stable.Locator != locator {
		t.Fatalf("stable symlink state = %#v", stable)
	}
}

func TestCaptureWakeSelfUpgradeStartupStateDoesNotLosePreCaptureFlip(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	oldTarget := writeWakeSelfUpgradeCandidate(t, dir, "old-amq")
	newTarget := writeWakeSelfUpgradeCandidate(t, dir, "new-amq")
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(oldTarget, locator); err != nil {
		t.Fatal(err)
	}
	running, err := captureWakeImageEvidence(oldTarget, fixture.lock.Lock.ImageVersion)
	if err != nil {
		t.Fatal(err)
	}
	replaceWakeSelfUpgradeLocator(t, locator, newTarget)
	state := captureWakeSelfUpgradeStartupState(locator, true, running)
	if !state.Eligible || state.Locator != locator {
		t.Fatalf("pre-capture flip state = %#v", state)
	}
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("pre-capture flip decision = %#v, want pending", decision)
	}
}

func TestProbeWakeSelfUpgradeLocatorRevalidatesUnresolvedOwner(t *testing.T) {
	dir := t.TempDir()
	target := writeWakeSelfUpgradeCandidate(t, dir, "candidate")
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(target, locator); err != nil {
		t.Fatal(err)
	}

	previousCurrentUID := wakeTargetCurrentUID
	previousOwnerUID := wakeTargetFileOwnerUID
	previousEval := wakeSelfUpgradeEvalSymlinks
	wakeTargetCurrentUID = func() (int, bool) { return 1000, true }
	wakeTargetFileOwnerUID = func(os.FileInfo) (int, bool) { return 2000, true }
	wakeSelfUpgradeEvalSymlinks = func(string) (string, error) {
		t.Fatal("unsafe unresolved locator was followed")
		return "", nil
	}
	t.Cleanup(func() {
		wakeTargetCurrentUID = previousCurrentUID
		wakeTargetFileOwnerUID = previousOwnerUID
		wakeSelfUpgradeEvalSymlinks = previousEval
	})

	if _, err := probeWakeSelfUpgradeLocator(locator); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("probe error=%v, want unresolved-owner refusal", err)
	}
}

func TestProbeWakeSelfUpgradeLocatorRejectsReplacementDuringResolution(t *testing.T) {
	dir := t.TempDir()
	first := writeWakeSelfUpgradeCandidate(t, dir, "first")
	second := writeWakeSelfUpgradeCandidate(t, dir, "second")
	locator := filepath.Join(dir, "amq")
	if err := os.Symlink(first, locator); err != nil {
		t.Fatal(err)
	}

	previousEval := wakeSelfUpgradeEvalSymlinks
	wakeSelfUpgradeEvalSymlinks = func(path string) (string, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			replaceWakeSelfUpgradeLocator(t, locator, second)
		}
		return resolved, err
	}
	t.Cleanup(func() { wakeSelfUpgradeEvalSymlinks = previousEval })

	if _, err := probeWakeSelfUpgradeLocator(locator); err == nil ||
		!strings.Contains(err.Error(), "changed while resolving") {
		t.Fatalf("probe error=%v, want locator-change refusal", err)
	}
}

func TestMaintainWakeSelfUpgradeUsesLiveProcessImageOverRecordedEvidence(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	state := selfUpgradeStateForCandidate(t, filepath.Clean(executable))
	stubWakeSelfUpgradeVersion(t, "99.0.0")
	if fixture.lock.Lock.RunningImageEvidence == nil {
		t.Fatal("fixture running image evidence is missing")
	}
	fixture.lock.Lock.RunningImageEvidence.Inode++

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionRefused ||
		!strings.Contains(decision.Reason, "not conclusively different from the live wake") {
		t.Fatalf("live image decision = %#v, want refused", decision)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("same live image created restart record: %v", err)
	}
}

func TestMaintainWakeSelfUpgradeRefusesUnknownLiveProcessImage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "99.0.0")
	fixture.lock.PID = 1 << 30

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionRefused ||
		!strings.Contains(decision.Reason, "unknown or ambiguous") {
		t.Fatalf("unknown live image decision = %#v, want refused", decision)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("unknown live image created restart record: %v", err)
	}
}

func TestMaintainWakeSelfUpgradeRetriesTransientVersionFailure(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	previous := wakeSelfUpgradeRunVersion
	attempts := 0
	wakeSelfUpgradeRunVersion = func(string) (string, error) {
		attempts++
		if attempts == 1 {
			return "", os.ErrNotExist
		}
		return fixture.lock.Lock.ImageVersion, nil
	}
	t.Cleanup(func() { wakeSelfUpgradeRunVersion = previous })

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionNoCandidate {
		t.Fatalf("transient decision=%#v err=%v", decision, err)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("transient version failure advanced the probe baseline")
	}
	decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionPrefilterRefused || attempts != 2 {
		t.Fatalf("retry decision=%#v attempts=%d err=%v", decision, attempts, err)
	}
}

func TestMaintainWakeSelfUpgradeRetriesCandidateAfterForeignRecordClears(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	foreign := fixture.record
	foreign.Source = wakeRestartSourceForeign
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, foreign)

	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionRestartPending {
		t.Fatalf("foreign-record decision=%#v err=%v", decision, err)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("foreign restart record advanced the probe baseline")
	}
	removeWakeRestartRecordForTest(t, fixture)

	decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("cleared-record decision=%#v err=%v", decision, err)
	}
	installed := readWakeSelfUpgradeRestartRecord(t, fixture)
	installedCandidate := wakeSelfUpgradeCandidateFromEvidence(installed.Candidate)
	if installed.Source != wakeRestartSourceSelf || decision.Candidate == nil ||
		*installedCandidate != *decision.Candidate {
		t.Fatalf("published record=%#v decision=%#v", installed, decision)
	}
}

func TestMaintainWakeSelfUpgradeRefusedMemorySurvivesTicks(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidatePath := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	candidate, err := captureWakeImageEvidence(candidatePath, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := newWakeRestartRequestID()
	if err != nil {
		t.Fatal(err)
	}
	stagePath, err := planWakeRestartStagePlatform(candidate, requestID)
	if err != nil {
		t.Fatal(err)
	}
	record := wakeRestartRecord{
		Schema:             wakeRestartSchemaV1,
		RequestID:          requestID,
		Status:             wakeRestartPending,
		Source:             wakeRestartSourceSelf,
		Root:               fixture.root,
		Agent:              fixture.agent,
		Generation:         fixture.lock.Lock.Generation,
		Owner:              fixture.owner,
		Candidate:          candidate,
		StagePath:          stagePath,
		PreviousBoundImage: previousDarwinWakeRestartStageForLock(fixture.lock.Lock),
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := refuseWakeRestartRecord(fixture.agentDir, record, "candidate preflight failed"); err != nil {
		t.Fatal(err)
	}

	decision, err := publishWakeSelfUpgradePending(
		fixture.agentDir,
		fixture.lock,
		wakeRestartRecord{
			Schema:     wakeRestartSchemaV1,
			RequestID:  "0123456789abcdef0123456789abcdef",
			Status:     wakeRestartPending,
			Source:     wakeRestartSourceSelf,
			Root:       fixture.root,
			Agent:      fixture.agent,
			Generation: fixture.lock.Lock.Generation,
			Owner:      fixture.owner,
			Candidate:  candidate,
		},
		wakeSelfUpgradeDecision{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionRefusedMemory {
		t.Fatalf("refused memory decision = %#v", decision)
	}

	state := wakeSelfUpgradeState{Enabled: true, Eligible: true, Locator: candidatePath}
	probe, err := probeWakeSelfUpgradeLocator(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.lastProbe = probe
	for tick := 0; tick < 3; tick++ {
		decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
		if err != nil || decision.Action != wakeSelfUpgradeActionUnchanged {
			t.Fatalf("tick %d decision=%#v err=%v", tick, decision, err)
		}
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		stored, exists, err := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists || stored.Status != wakeRestartRefused || stored.Source != wakeRestartSourceSelf {
			return os.ErrNotExist
		}
		return nil
	}); err != nil {
		t.Fatalf("refused memory did not survive ticks: %v", err)
	}
}

func TestWakeSelfUpgradeDiagnosticTreatsCorruptionAsNoData(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	path := filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName)
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		if _, exists := readWakeSelfUpgradeDiagnosticAt(dirfd, fixture.agentDir, fixture.lock); exists {
			t.Fatal("corrupt diagnostic was treated as data")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordWakeSelfUpgradeDecisionWritesOnlyOnChange(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	state := wakeSelfUpgradeState{Enabled: true, Eligible: true, Locator: "/opt/homebrew/bin/amq"}
	decision := wakeSelfUpgradeDecision{Action: wakeSelfUpgradeActionPrefilterRefused, Reason: "same version"}
	previousNow := wakeSelfUpgradeNow
	clock := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	wakeSelfUpgradeNow = func() time.Time { return clock }
	t.Cleanup(func() { wakeSelfUpgradeNow = previousNow })

	if err := recordWakeSelfUpgradeDecision(fixture.agentDir, fixture.lock, state, decision); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName))
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := recordWakeSelfUpgradeDecision(fixture.agentDir, fixture.lock, state, decision); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !sameWakeFileIdentity(first, second) {
		t.Fatal("unchanged diagnostic was rewritten")
	}
	decision.Reason = "downgrade"
	if err := recordWakeSelfUpgradeDecision(fixture.agentDir, fixture.lock, state, decision); err != nil {
		t.Fatal(err)
	}
	third, err := os.Stat(filepath.Join(fixture.agentDir.path, wakeSelfUpgradeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if sameWakeFileIdentity(second, third) {
		t.Fatal("changed diagnostic was not rewritten")
	}
}

func TestWakeSelfUpgradeDisabledByEnv(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(envWakeNoSelfUpgrade, value)
			if !wakeSelfUpgradeDisabledByEnv() {
				t.Fatalf("%q did not disable self-upgrade", value)
			}
		})
	}
}
