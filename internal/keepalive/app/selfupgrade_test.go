package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

func TestSelfUpgradeExecsStrictlyNewerImageAtQuiescentBoundary(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeCaptureCandidate = func(path string) (selfupgrade.ImageEvidence, error) {
		return captureSelfUpgradeTestImage(t, path, "1.1.0"), nil
	}
	var got selfupgrade.ImageEvidence
	var gotArgv, gotEnv []string
	selfUpgradeExecImage = func(candidate selfupgrade.ImageEvidence, argv, env []string) error {
		got = candidate
		gotArgv = append([]string(nil), argv...)
		gotEnv = append([]string(nil), env...)
		return nil
	}

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v", err)
	}
	if got.EmbeddedVersion != "1.1.0" {
		t.Fatalf("executed candidate version = %q, want 1.1.0", got.EmbeddedVersion)
	}
	if len(gotArgv) == 0 || len(gotEnv) == 0 {
		t.Fatalf("exec did not receive process argv/env: argv=%#v env=%#v", gotArgv, gotEnv)
	}
	if controller.lastObservation == nil || controller.lastObservation.action != selfUpgradeActionExec {
		t.Fatalf("last observation = %#v, want exec", controller.lastObservation)
	}
	assertSelfUpgradeStateMode(t, controller.statePath)
}

func TestSelfUpgradeSchema1MigratesBeforeProtectedExec(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	attempt := selfupgrade.NewAttempt(candidate, time.Now())
	legacy := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchemaV1,
		Generation:       "legacy-generation",
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
		Attempt:          &attempt,
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.statePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadSelfUpgradeState(controller); err != nil {
		t.Fatalf("load schema 1 state: %v", err)
	}
	if len(controller.attempts) != 1 || controller.attempts[0] != attempt {
		t.Fatalf("migrated attempts = %#v, want %#v", controller.attempts, attempt)
	}
	if controller.statePublished {
		t.Fatal("schema 1 state was treated as schema 2 before publication")
	}
	if err := controller.ensureStatePublished(); err != nil {
		t.Fatalf("publish migrated state: %v", err)
	}
	state := readSelfUpgradeStateForTest(t, controller.statePath)
	if state.Schema != selfUpgradeStateSchema || len(state.Attempts) != 1 || state.Attempt != nil {
		t.Fatalf("published migrated state = %#v, want schema 2 ledger", state)
	}
}

func TestSelfUpgradePublicationMergesConcurrentAttemptAndRefusalLedgers(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidateBPath := filepath.Join(dir, "candidate-b")
	candidateCPath := filepath.Join(dir, "candidate-c")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidateBPath, "candidate b")
	writeExecutableForSelfUpgradeTest(t, candidateCPath, "candidate c")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidateB := captureSelfUpgradeTestImage(t, candidateBPath, "1.1.0")
	candidateC := captureSelfUpgradeTestImage(t, candidateCPath, "1.2.0")
	first := testSelfUpgradeController(dir, candidateBPath, incumbent)
	second := testSelfUpgradeController(dir, candidateCPath, incumbent)
	first.generation = "shared-generation"
	second.generation = first.generation
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return time.Unix(1_700_000_001, 0) }
	t.Cleanup(func() { selfUpgradeNow = previousNow })

	release, err := first.recordAttempt(candidateB)
	if err != nil {
		t.Fatalf("record first attempt: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release first attempt lock: %v", err)
	}
	if err := second.refuse(candidateC, errors.New("test refusal")); !strings.Contains(err.Error(), "test refusal") {
		t.Fatalf("refuse second candidate error = %v, want cause", err)
	}
	state := readSelfUpgradeStateForTest(t, first.statePath)
	if len(state.Attempts) != 1 || !state.Attempts[0].Matches(candidateB) {
		t.Fatalf("merged attempts = %#v, want candidate B preserved", state.Attempts)
	}
	if !selfupgrade.RefusedCandidatesContain(state.RefusedCandidates, candidateC) {
		t.Fatalf("merged refusals = %#v, want candidate C", state.RefusedCandidates)
	}
	info, err := os.Stat(filepath.Join(dir, selfUpgradeStateLockName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state lock mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSelfUpgradeFutureAttemptTimestampDisablesEligibility(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	now := time.Unix(1_700_000_000, 0).UTC()
	future := selfupgrade.NewAttempt(candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second))
	state := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchema,
		Generation:       controller.generation,
		Attempts:         []selfupgrade.Attempt{future},
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.statePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { selfUpgradeNow = previousNow })
	if err := loadSelfUpgradeState(controller); err != nil {
		t.Fatalf("load future attempt: %v", err)
	}
	if controller.eligible || !strings.Contains(controller.reason, "timestamp is uncertain") {
		t.Fatalf("future attempt controller = %#v, want unavailable", controller)
	}
	if len(controller.attempts) != 1 || controller.attempts[0] != future {
		t.Fatalf("future attempt ledger = %#v, want preserved record", controller.attempts)
	}
}

func TestSelfUpgradeRecordsAttemptBeforeExecAndSettles(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousNow := selfUpgradeNow
	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	selfUpgradeNow = func() time.Time { return time.Unix(1_700_000_000, 0) }
	selfUpgradeCaptureCandidate = func(path string) (selfupgrade.ImageEvidence, error) {
		return captureSelfUpgradeTestImage(t, path, "1.1.0"), nil
	}
	var attempted selfupgrade.ImageEvidence
	selfUpgradeExecImage = func(candidate selfupgrade.ImageEvidence, _ []string, _ []string) error {
		attempted = candidate
		state := readSelfUpgradeStateForTest(t, controller.statePath)
		if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusAttempt {
			t.Fatalf("state at exec = %#v, want unsettled attempt", state)
		}
		return nil
	}
	t.Cleanup(func() {
		selfUpgradeNow = previousNow
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v", err)
	}
	if len(controller.attempts) != 1 || controller.attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("controller attempts = %#v, want one unsettled attempt", controller.attempts)
	}
	if err := controller.markSettled(); err != nil {
		t.Fatalf("markSettled() with old incumbent error = %v", err)
	}
	state := readSelfUpgradeStateForTest(t, controller.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("old-incumbent state = %#v, want unsettled attempt", state)
	}
	controller.incumbent = attempted
	if err := controller.markSettled(); err != nil {
		t.Fatalf("markSettled() with attempted incumbent error = %v", err)
	}
	state = readSelfUpgradeStateForTest(t, controller.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("settled state = %#v, want settled attempt", state)
	}
}

func TestSelfUpgradeUnsettledAttemptMatchingCandidateRefusesAndStaysPending(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	attempt := selfupgrade.NewAttempt(candidate, time.Unix(1_700_000_000, 0))
	controller.attempts = []selfupgrade.Attempt{attempt}
	if err := controller.saveState(); err != nil {
		t.Fatalf("save attempt state: %v", err)
	}

	previousNow := selfUpgradeNow
	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	selfUpgradeNow = func() time.Time { return time.Unix(1_700_000_001, 0) }
	selfUpgradeCaptureCandidate = func(path string) (selfupgrade.ImageEvidence, error) {
		return captureSelfUpgradeTestImage(t, path, "1.1.0"), nil
	}
	execCalls := 0
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		execCalls++
		return nil
	}
	t.Cleanup(func() {
		selfUpgradeNow = previousNow
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})

	err := controller.maintain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "replacement attempt was armed at 2023-11-14T22:13:20Z") {
		t.Fatalf("maintain() error = %v, want unsettled-attempt refusal", err)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want no retry", execCalls)
	}
	if controller.lastObservation == nil || controller.lastObservation.action != selfUpgradeActionRefused {
		t.Fatalf("last observation = %#v, want refusal", controller.lastObservation)
	}
	if len(controller.attempts) != 1 || controller.attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("controller attempts = %#v, want one unsettled attempt", controller.attempts)
	}
	if err := controller.markSettled(); err != nil {
		t.Fatalf("markSettled() with old incumbent error = %v", err)
	}
	state := readSelfUpgradeStateForTest(t, controller.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("persisted attempt = %#v, want unsettled attempt", state)
	}
	if !selfupgrade.RefusedCandidatesContain(state.RefusedCandidates, candidate) {
		t.Fatalf("persisted refusals = %#v, want candidate", state.RefusedCandidates)
	}
}

func TestSelfUpgradeOptOutDoesNotPublishOrProbe(t *testing.T) {
	dir := t.TempDir()
	controller := &selfUpgradeController{
		enabled:   false,
		eligible:  false,
		statePath: filepath.Join(dir, selfUpgradeStateFileName),
	}
	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeCaptureCandidate = func(string) (selfupgrade.ImageEvidence, error) {
		t.Fatal("opt-out should not probe the candidate")
		return selfupgrade.ImageEvidence{}, nil
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("opt-out should not exec a candidate")
		return nil
	}

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v", err)
	}
	if _, err := os.Stat(controller.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opt-out state path error = %v, want no state file", err)
	}
}

func testSelfUpgradeController(dir, locator string, incumbent selfupgrade.ImageEvidence) *selfUpgradeController {
	return &selfUpgradeController{
		enabled:    true,
		eligible:   true,
		locator:    locator,
		incumbent:  incumbent,
		generation: "test-generation",
		statePath:  filepath.Join(dir, selfUpgradeStateFileName),
	}
}

func writeExecutableForSelfUpgradeTest(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func captureSelfUpgradeTestImage(t *testing.T, path, version string) selfupgrade.ImageEvidence {
	t.Helper()
	evidence, err := selfupgrade.CaptureImageEvidence(path, version)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func assertSelfUpgradeStateMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func readSelfUpgradeStateForTest(t *testing.T, path string) selfUpgradeStateFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state selfUpgradeStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Restored from origin/main during the round-2 cull: this is the only test that
// exercises newSelfUpgradeController's real init path (canonical/resolved path
// resolution, generation, and the startup refusal contract that the locator must
// name the running image). Fail-closed identity contract.
func TestSelfUpgradeRejectsLocatorNotRunningImage(t *testing.T) {
	dir := t.TempDir()
	runningPath := filepath.Join(dir, "running")
	locatorPath := filepath.Join(dir, "locator")
	writeExecutableForSelfUpgradeTest(t, runningPath, "running image")
	writeExecutableForSelfUpgradeTest(t, locatorPath, "candidate image")

	previousExecutable := selfUpgradeExecutable
	t.Cleanup(func() { selfUpgradeExecutable = previousExecutable })
	selfUpgradeExecutable = func() (string, error) { return runningPath, nil }

	controller := newSelfUpgradeController(filepath.Join(dir, "registry.json"), locatorPath, "1.0.0", true)
	if controller.eligible {
		t.Fatal("controller eligible for a locator that does not name the running image")
	}
	if controller.reason != "self-upgrade locator does not name the running image" {
		t.Fatalf("controller reason = %q, want exact locator identity refusal", controller.reason)
	}
}
