//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	previous := wakeSelfUpgradeCaptureCandidate
	wakeSelfUpgradeCaptureCandidate = func(path string) (wakeImageEvidenceV1, error) {
		return captureWakeImageEvidence(path, version)
	}
	t.Cleanup(func() { wakeSelfUpgradeCaptureCandidate = previous })
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
	// Darwin cannot corroborate a recorded/live identity mismatch after the
	// pathname may have been unlinked; Linux's /proc/<pid>/exe identity is
	// conclusive and correctly refuses a same-image candidate.
	if runtime.GOOS == "darwin" {
		if decision.Action != wakeSelfUpgradeActionDeferred ||
			!strings.Contains(decision.Reason, "unknown or ambiguous") {
			t.Fatalf("live image decision = %#v, want deferred", decision)
		}
	} else if decision.Action != wakeSelfUpgradeActionRefused ||
		!strings.Contains(decision.Reason, "not conclusively different from the live wake") {
		t.Fatalf("live image decision = %#v, want refused", decision)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("same live image created restart record: %v", err)
	}
}

func TestMaintainWakeSelfUpgradeDefersUnknownLiveProcessImage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	stubWakeSelfUpgradeVersion(t, "99.0.0")
	fixture.lock.PID = 1 << 30

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionDeferred ||
		!strings.Contains(decision.Reason, "unknown or ambiguous") {
		t.Fatalf("unknown live image decision = %#v, want deferred", decision)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("unknown live image advanced the probe baseline")
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("unknown live image created restart record: %v", err)
	}

	decision, err = maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != wakeSelfUpgradeActionDeferred {
		t.Fatalf("retry after unknown live image = %#v, want deferred not unchanged/refused", decision)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("retry after unknown live image advanced the probe baseline")
	}
}

func TestMaintainWakeSelfUpgradeRetriesTransientVersionFailure(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	previous := wakeSelfUpgradeCaptureCandidate
	attempts := 0
	wakeSelfUpgradeCaptureCandidate = func(path string) (wakeImageEvidenceV1, error) {
		attempts++
		if attempts == 1 {
			return wakeImageEvidenceV1{}, os.ErrNotExist
		}
		return captureWakeImageEvidence(path, fixture.lock.Lock.ImageVersion)
	}
	t.Cleanup(func() { wakeSelfUpgradeCaptureCandidate = previous })

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
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
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

func TestPublishWakeSelfUpgradePendingCarriesLegacyRefusalMemory(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	dir := t.TempDir()
	pathA := writeWakeSelfUpgradeCandidate(t, dir, "candidate-a")
	pathB := writeWakeSelfUpgradeCandidate(t, dir, "candidate-b")
	evidenceA, err := captureWakeImageEvidence(pathA, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	evidenceB, err := captureWakeImageEvidence(pathB, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	legacy := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		Source:     wakeRestartSourceSelf,
		RequestID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:     wakeRestartRefused,
		Root:       fixture.root,
		Agent:      fixture.agent,
		Generation: fixture.lock.Lock.Generation,
		Owner:      fixture.owner,
		Candidate:  evidenceA,
		Reason:     "legacy refusal",
	}
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, legacy)
	}); err != nil {
		t.Fatal(err)
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

	sameA := legacy
	sameA.RequestID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sameA.Status = wakeRestartPending
	sameA.Reason = ""
	decision, err := publishWakeSelfUpgradePending(
		fixture.agentDir,
		fixture.lock,
		sameA,
		wakeSelfUpgradeDecision{},
	)
	if err != nil || decision.Action != wakeSelfUpgradeActionRefusedMemory {
		t.Fatalf("legacy same-A decision=%#v err=%v", decision, err)
	}
	if got := quarantineCount(); got != 0 {
		t.Fatalf("legacy same-A quarantine count=%d, want 0", got)
	}
	if got := readWakeSelfUpgradeRestartRecord(t, fixture); !sameWakeRestartRecord(got, legacy) {
		t.Fatalf("legacy same-A changed active record: got=%#v want=%#v", got, legacy)
	}

	pendingB := sameA
	pendingB.RequestID = "cccccccccccccccccccccccccccccccc"
	pendingB.Candidate = evidenceB
	decision, err = publishWakeSelfUpgradePending(
		fixture.agentDir,
		fixture.lock,
		pendingB,
		wakeSelfUpgradeDecision{},
	)
	if err != nil || decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("legacy A to B decision=%#v err=%v", decision, err)
	}
	installedB := readWakeSelfUpgradeRestartRecord(t, fixture)
	if installedB.Status != wakeRestartPending || len(installedB.RefusedCandidates) != 1 ||
		!wakeSelfUpgradeRefusedCandidatesContain(installedB.RefusedCandidates, evidenceA) ||
		wakeSelfUpgradeRefusedCandidatesContain(installedB.RefusedCandidates, evidenceB) {
		t.Fatalf("carried legacy refusal memory = %#v", installedB)
	}
	if got := quarantineCount(); got != 0 {
		t.Fatalf("same-scope legacy A to B quarantine count=%d, want 0", got)
	}
	if err := refuseWakeRestartRecord(fixture.agentDir, installedB, "B failed"); err != nil {
		t.Fatal(err)
	}
	refusedB := readWakeSelfUpgradeRestartRecord(t, fixture)
	if len(refusedB.RefusedCandidates) != 2 ||
		!wakeSelfUpgradeRefusedCandidatesContain(refusedB.RefusedCandidates, evidenceA) ||
		!wakeSelfUpgradeRefusedCandidatesContain(refusedB.RefusedCandidates, evidenceB) {
		t.Fatalf("B refusal memory = %#v", refusedB)
	}
}

func TestPublishWakeSelfUpgradePendingResetsRefusalsForNewGeneration(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	path := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate-a")
	evidence, err := captureWakeImageEvidence(path, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	old := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		Source:     wakeRestartSourceSelf,
		RequestID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:     wakeRestartRefused,
		Root:       fixture.root,
		Agent:      fixture.agent,
		Generation: fixture.lock.Lock.Generation,
		Owner:      fixture.owner,
		Candidate:  evidence,
		RefusedCandidates: []wakeSelfUpgradeRefusedCandidate{
			wakeSelfUpgradeRefusedCandidateFromEvidence(evidence),
		},
		Reason: "old generation refusal",
	}
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, old)
	}); err != nil {
		t.Fatal(err)
	}

	nextLock := fixture.lock.Lock
	nextLock.Generation = "dddddddddddddddddddddddddddddddd"
	writeWakeLockForTest(t, fixture.root, fixture.agent, nextLock)
	nextInspection := inspectWakeLock(fixture.root, fixture.agent)
	if !nextInspection.IdentityConfirmed || nextInspection.Lock.Generation != nextLock.Generation {
		t.Fatalf("next-generation lock = %#v", nextInspection)
	}
	next := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		Source:     wakeRestartSourceSelf,
		RequestID:  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Status:     wakeRestartPending,
		Root:       fixture.root,
		Agent:      fixture.agent,
		Generation: nextLock.Generation,
		Owner:      fixture.owner,
		Candidate:  evidence,
	}
	decision, err := publishWakeSelfUpgradePending(
		fixture.agentDir,
		nextInspection,
		next,
		wakeSelfUpgradeDecision{},
	)
	if err != nil || decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("new-generation decision=%#v err=%v", decision, err)
	}
	installed := readWakeSelfUpgradeRestartRecord(t, fixture)
	if installed.Generation != nextLock.Generation || len(installed.RefusedCandidates) != 0 {
		t.Fatalf("new generation inherited refusal memory: %#v", installed)
	}
}

func TestSameWakeSelfUpgradeRefusalScope(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartRefused
	record.Reason = "candidate refused"
	record.PreviousBoundImage = previousDarwinWakeRestartStageForLock(fixture.lock.Lock)

	if !sameWakeSelfUpgradeRefusalScope(record, fixture.lock) {
		t.Fatal("matching refusal scope was rejected")
	}

	tests := []struct {
		name   string
		mutate func(*wakeRestartRecord)
	}{
		{name: "root", mutate: func(record *wakeRestartRecord) { record.Root += "-other" }},
		{name: "agent", mutate: func(record *wakeRestartRecord) { record.Agent = "claude" }},
		{name: "generation", mutate: func(record *wakeRestartRecord) { record.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{name: "owner", mutate: func(record *wakeRestartRecord) { record.Owner.SessionID++ }},
		{name: "previous bound image", mutate: func(record *wakeRestartRecord) {
			if record.PreviousBoundImage == nil {
				changed := fixture.candidate
				record.PreviousBoundImage = &changed
				return
			}
			changed := *record.PreviousBoundImage
			changed.Inode++
			record.PreviousBoundImage = &changed
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mismatched := record
			test.mutate(&mismatched)
			if sameWakeSelfUpgradeRefusalScope(mismatched, fixture.lock) {
				t.Fatal("mismatched refusal scope was accepted")
			}
		})
	}
}

func TestRememberWakeSelfUpgradeRefusalRetainsEightMostRecentDistinct(t *testing.T) {
	template := newWakeRestartFixture(t).candidate
	var remembered []wakeSelfUpgradeRefusedCandidate
	var evidence []wakeImageEvidenceV1
	for index := 0; index < wakeSelfUpgradeRefusalLimit+2; index++ {
		candidate := template
		candidate.Inode = uint64(index + 1)
		evidence = append(evidence, candidate)
		remembered = rememberWakeSelfUpgradeRefusal(remembered, candidate)
	}
	if len(remembered) != wakeSelfUpgradeRefusalLimit {
		t.Fatalf("remembered=%d, want %d", len(remembered), wakeSelfUpgradeRefusalLimit)
	}
	for _, evicted := range evidence[:2] {
		if wakeSelfUpgradeRefusedCandidatesContain(remembered, evicted) {
			t.Fatalf("old refusal was not evicted: %#v", evicted)
		}
	}
	for _, retained := range evidence[2:] {
		if !wakeSelfUpgradeRefusedCandidatesContain(remembered, retained) {
			t.Fatalf("recent refusal was not retained: %#v", retained)
		}
	}
	remembered = rememberWakeSelfUpgradeRefusal(remembered, evidence[2])
	if len(remembered) != wakeSelfUpgradeRefusalLimit ||
		remembered[len(remembered)-1] != wakeSelfUpgradeRefusedCandidateFromEvidence(evidence[2]) {
		t.Fatalf("repeat refusal did not move to most recent: %#v", remembered)
	}
}

func TestValidateWakeRestartRecordRefusedCandidates(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	current := wakeSelfUpgradeRefusedCandidateFromEvidence(record.Candidate)
	prior := current
	prior.Inode++

	valid := record
	valid.RefusedCandidates = []wakeSelfUpgradeRefusedCandidate{prior}
	if err := validateWakeRestartRecord(valid); err != nil {
		t.Fatalf("valid pending history: %v", err)
	}
	encoded, err := json.Marshal(valid.RefusedCandidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"execution_path", "method", "ctime"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("path-free refusal identity contains %q: %s", forbidden, encoded)
		}
	}

	tests := []struct {
		name   string
		mutate func(*wakeRestartRecord)
	}{
		{
			name: "foreign source",
			mutate: func(record *wakeRestartRecord) {
				record.Source = wakeRestartSourceForeign
			},
		},
		{
			name: "duplicate",
			mutate: func(record *wakeRestartRecord) {
				record.RefusedCandidates = append(record.RefusedCandidates, prior)
			},
		},
		{
			name: "over limit",
			mutate: func(record *wakeRestartRecord) {
				record.RefusedCandidates = nil
				for index := 0; index <= wakeSelfUpgradeRefusalLimit; index++ {
					candidate := prior
					candidate.Inode = uint64(index + 1)
					record.RefusedCandidates = append(record.RefusedCandidates, candidate)
				}
			},
		},
		{
			name: "pending current candidate",
			mutate: func(record *wakeRestartRecord) {
				record.RefusedCandidates = []wakeSelfUpgradeRefusedCandidate{current}
			},
		},
		{
			name: "refused current missing",
			mutate: func(record *wakeRestartRecord) {
				record.Status = wakeRestartRefused
				record.Reason = "test refusal"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.RefusedCandidates = append(
				[]wakeSelfUpgradeRefusedCandidate(nil),
				valid.RefusedCandidates...,
			)
			test.mutate(&candidate)
			if err := validateWakeRestartRecord(candidate); err == nil {
				t.Fatalf("validateWakeRestartRecord(%s) error=nil", test.name)
			}
		})
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

// TestMaintainWakeSelfUpgradeSupersedesRefusedOperatorRecord is the M2 contract:
// a refused record with an empty (operator) source is terminal; a new self
// candidate reclaims and quarantines it and publishes a self pending record,
// rather than preserving it as restart_present and re-probing forever.
func TestMaintainWakeSelfUpgradeSupersedesRefusedOperatorRecord(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	foreign := fixture.record
	foreign.Source = "" // operator `amq wake restart` refused record has an empty source
	foreign.Status = wakeRestartRefused
	foreign.Reason = "operator restart refused"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, foreign)

	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatalf("maintainWakeSelfUpgrade error = %v", err)
	}
	if decision.Action == wakeSelfUpgradeActionRestartPresent {
		t.Fatalf("refused operator record was preserved (restart_present) instead of superseded: %#v", decision)
	}
	// A new self candidate must publish a self pending record.
	installed := readWakeSelfUpgradeRestartRecord(t, fixture)
	if installed.Source != wakeRestartSourceSelf || installed.Status != wakeRestartPending {
		t.Fatalf("published record = %#v; want self/pending", installed)
	}
	quarantined, err := filepath.Glob(filepath.Join(fixture.agentDir.path, wakeRestartFileName+".quarantined.*"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine paths=%v err=%v; want 1", quarantined, err)
	}
}

// TestMaintainWakeSelfUpgradeSupersedesRefusedForeignRecord is the M2 contract
// for an explicit foreign source: a refused foreign record is also superseded
// (not preserved), and no refusal memory is inherited from it.
func TestMaintainWakeSelfUpgradeSupersedesRefusedForeignRecord(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	foreign := fixture.record
	foreign.Source = wakeRestartSourceForeign
	foreign.Status = wakeRestartRefused
	foreign.Reason = "foreign restart refused"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, foreign)

	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil {
		t.Fatalf("maintainWakeSelfUpgrade error = %v", err)
	}
	if decision.Action == wakeSelfUpgradeActionRestartPresent {
		t.Fatalf("refused foreign record was preserved (restart_present) instead of superseded: %#v", decision)
	}
	installed := readWakeSelfUpgradeRestartRecord(t, fixture)
	if installed.Source != wakeRestartSourceSelf || installed.Status != wakeRestartPending {
		t.Fatalf("published record = %#v; want self/pending", installed)
	}
	// No refusal memory inherited from a non-self record.
	if len(installed.RefusedCandidates) != 0 {
		t.Fatalf("refusal memory inherited from a foreign record: %#v", installed.RefusedCandidates)
	}
	quarantined, err := filepath.Glob(filepath.Join(fixture.agentDir.path, wakeRestartFileName+".quarantined.*"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine paths=%v err=%v; want 1", quarantined, err)
	}
}

// TestMaintainWakeSelfUpgradePreservesPendingForeignRecord is the M2 negative:
// a PENDING foreign record is still preserved (restart_present), untouched —
// only refused records of any source are superseded.
func TestMaintainWakeSelfUpgradePreservesPendingForeignRecord(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	foreign := fixture.record
	foreign.Source = wakeRestartSourceForeign
	foreign.Status = wakeRestartPending
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, foreign)

	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionRestartPending {
		t.Fatalf("pending foreign record decision=%#v err=%v; want restart_pending", decision, err)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("pending foreign restart record advanced the probe baseline")
	}
	untouched := readWakeSelfUpgradeRestartRecord(t, fixture)
	if untouched.Source != wakeRestartSourceForeign || untouched.Status != wakeRestartPending {
		t.Fatalf("pending foreign record mutated: %#v", untouched)
	}
}

func TestMaintainWakeSelfUpgradePreservesClaimedSchema2Record(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	claimed := fixture.record
	claimed.Schema = wakeRestartSchemaV2
	claimed.Status = wakeRestartPending
	claimed.SuccessorGeneration = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, claimed)

	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	initialProbe := state.lastProbe
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionRestartPending {
		t.Fatalf("claimed schema-2 record decision=%#v err=%v; want restart_pending", decision, err)
	}
	if state.lastProbe != initialProbe {
		t.Fatal("claimed schema-2 record advanced the probe baseline")
	}
	untouched := readWakeSelfUpgradeRestartRecord(t, fixture)
	if untouched.Schema != wakeRestartSchemaV2 || untouched.Status != wakeRestartPending ||
		untouched.SuccessorGeneration != claimed.SuccessorGeneration {
		t.Fatalf("claimed schema-2 record mutated: %#v", untouched)
	}
}

// TestRefuseWakeRestartRecordNeverProducesSchema2 is the L1 contract:
// refuseWakeRestartRecord only operates on schema-1 pending records and the
// refused record it writes stays schema 1. Schema 2 is a reserved (accepted on
// read, never emitted) combination.
func TestRefuseWakeRestartRecordNeverProducesSchema2(t *testing.T) {
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
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := refuseWakeRestartRecord(fixture.agentDir, record, "L1 schema probe"); err != nil {
		t.Fatal(err)
	}
	refused := readWakeSelfUpgradeRestartRecord(t, fixture)
	if refused.Schema != wakeRestartSchemaV1 {
		t.Fatalf("refused record schema = %d; want %d (schema 2 is reserved, never emitted)", refused.Schema, wakeRestartSchemaV1)
	}
	if refused.Status != wakeRestartRefused {
		t.Fatalf("refused record status = %q; want refused", refused.Status)
	}
}
