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

func TestSelfUpgradeDoesNotExecIdenticalBytesWithDifferentVersion(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	contents := "same image"
	writeExecutableForSelfUpgradeTest(t, incumbentPath, contents)
	writeExecutableForSelfUpgradeTest(t, candidatePath, contents)
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeCaptureCandidate = func(string) (selfupgrade.ImageEvidence, error) {
		t.Fatal("identical image should not require candidate capture")
		return selfupgrade.ImageEvidence{}, nil
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("identical image must not be executed")
		return nil
	}

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v", err)
	}
	if controller.lastObservation == nil || controller.lastObservation.action != selfUpgradeActionUnchanged {
		t.Fatalf("last observation = %#v, want unchanged", controller.lastObservation)
	}
}

func TestSelfUpgradeDefersFailedCandidateBuildInfoRead(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "good image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "corrupt image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})
	captureCalls := 0
	selfUpgradeCaptureCandidate = func(string) (selfupgrade.ImageEvidence, error) {
		captureCalls++
		return selfupgrade.ImageEvidence{}, errors.New("candidate build info unavailable")
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("corrupt candidate must not be executed")
		return nil
	}

	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "deferred for candidate "+candidatePath) {
		t.Fatalf("first maintain() error = %v, want deferred build-info error", err)
	}
	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "deferred for candidate "+candidatePath) {
		t.Fatalf("second maintain() error = %v, want deferred build-info error", err)
	}
	if captureCalls != 2 {
		t.Fatalf("candidate capture calls = %d, want retry after deferred failure", captureCalls)
	}
	if len(controller.refused) != 0 {
		t.Fatalf("refusal memory length = %d, want no refusal for an inconclusive probe", len(controller.refused))
	}
	assertSelfUpgradeStateMode(t, controller.statePath)
}

func TestSelfUpgradeDoesNotReprobeSameCandidateIdentity(t *testing.T) {
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
	captureCalls := 0
	selfUpgradeCaptureCandidate = func(path string) (selfupgrade.ImageEvidence, error) {
		captureCalls++
		return captureSelfUpgradeTestImage(t, path, "1.1.0"), nil
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error { return nil }

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("first maintain() error = %v", err)
	}
	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("second maintain() error = %v", err)
	}
	if captureCalls != 1 {
		t.Fatalf("candidate capture calls = %d, want one for one candidate identity", captureCalls)
	}
}

func TestSelfUpgradeExecFailureIsRememberedOnce(t *testing.T) {
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
	execCalls := 0
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		execCalls++
		return errors.New("exec denied")
	}

	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "exec newer self-upgrade image") {
		t.Fatalf("first maintain() error = %v, want exec refusal", err)
	}
	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("second maintain() error = %v, want cached refusal", err)
	}
	if execCalls != 1 || len(controller.refused) != 1 {
		t.Fatalf("exec calls=%d refusal memory=%d, want 1 and 1", execCalls, len(controller.refused))
	}
	state := readSelfUpgradeStateForTest(t, controller.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("persisted attempts = %#v, want failed replacement attempt preserved", state.Attempts)
	}
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

func TestSelfUpgradeRecordAttemptRefusesFreshCandidateUnderLock(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	first := testSelfUpgradeController(dir, candidatePath, incumbent)
	second := testSelfUpgradeController(dir, candidatePath, incumbent)
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	t.Cleanup(func() { selfUpgradeNow = previousNow })

	release, err := first.recordAttempt(candidate)
	if err != nil {
		t.Fatalf("record first attempt: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		secondRelease, secondErr := second.recordAttempt(candidate)
		if secondRelease != nil {
			secondErr = errors.Join(secondErr, secondRelease())
		}
		result <- secondErr
	}()
	select {
	case secondErr := <-result:
		t.Fatalf("second recordAttempt completed while first held the lock: %v", secondErr)
	case <-time.After(100 * time.Millisecond):
	}
	if err := release(); err != nil {
		t.Fatalf("release first attempt lock: %v", err)
	}
	select {
	case secondErr := <-result:
		var refusal selfUpgradeAttemptRefusalError
		if !errors.As(secondErr, &refusal) {
			t.Fatalf("second recordAttempt error = %v, want authoritative refusal", secondErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second recordAttempt did not finish after lock release")
	}
	state := readSelfUpgradeStateForTest(t, first.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].UnixTime != 1_700_000_000 {
		t.Fatalf("attempt ledger = %#v, want one original attempt", state.Attempts)
	}
}

func TestSelfUpgradeAttemptSidecarSurvivesLegacyStateWriter(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	writer := testSelfUpgradeController(dir, candidatePath, incumbent)
	release, err := writer.recordAttempt(candidate)
	if err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release attempt lock: %v", err)
	}

	legacy := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchemaV1,
		Generation:       "legacy-generation",
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writer.statePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := testSelfUpgradeController(dir, candidatePath, incumbent)
	if err := loadSelfUpgradeState(reader); err != nil {
		t.Fatalf("load state after legacy writer: %v", err)
	}
	if len(reader.attempts) != 1 || !reader.attempts[0].Matches(candidate) {
		t.Fatalf("attempts after legacy writer = %#v, want preserved candidate", reader.attempts)
	}
	if err := reader.ensureStatePublished(); err != nil {
		t.Fatalf("republish migrated state: %v", err)
	}
	state := readSelfUpgradeStateForTest(t, reader.statePath)
	if state.Schema != selfUpgradeStateSchema || len(state.Attempts) != 1 || !state.Attempts[0].Matches(candidate) {
		t.Fatalf("republished state = %#v, want preserved attempt", state)
	}
	info, err := os.Stat(selfUpgradeAttemptsPath(reader.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt sidecar mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSelfUpgradeExistingSchema2StateGetsAttemptSidecar(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	state := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchema,
		Generation:       controller.generation,
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
	if err := loadSelfUpgradeState(controller); err != nil {
		t.Fatalf("load schema 2 state without sidecar: %v", err)
	}
	if controller.statePublished {
		t.Fatal("schema 2 state without an attempt sidecar was treated as fully published")
	}
	if err := controller.ensureStatePublished(); err != nil {
		t.Fatalf("publish missing attempt sidecar: %v", err)
	}
	info, err := os.Stat(selfUpgradeAttemptsPath(controller.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt sidecar mode = %o, want 600", info.Mode().Perm())
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

func TestSelfUpgradeFutureAttemptAfterMatchingAttemptDisablesEligibility(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	now := time.Unix(1_700_000_000, 0).UTC()
	state := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchema,
		Generation:       controller.generation,
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
		Attempts: []selfupgrade.Attempt{
			selfupgrade.NewAttempt(incumbent, now.Add(-time.Minute)),
			selfupgrade.NewAttempt(candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second)),
		},
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
		t.Fatalf("load ordered attempts: %v", err)
	}
	if controller.eligible || !strings.Contains(controller.reason, "timestamp is uncertain") {
		t.Fatalf("ordered future attempt controller = %#v, want unavailable", controller)
	}
	if len(controller.attempts) != 2 {
		t.Fatalf("ordered attempt ledger = %#v, want both attempts preserved", controller.attempts)
	}
}

func TestSelfUpgradeRefreshRejectsFutureAttemptAddedAfterStartup(t *testing.T) {
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
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { selfUpgradeNow = previousNow })
	state := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchema,
		Generation:       controller.generation,
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
		Attempts:         []selfupgrade.Attempt{future},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.statePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	previousCandidate := selfUpgradeCaptureCandidate
	selfUpgradeCaptureCandidate = func(string) (selfupgrade.ImageEvidence, error) {
		t.Fatal("future attempt should stop maintenance before candidate capture")
		return selfupgrade.ImageEvidence{}, nil
	}
	t.Cleanup(func() { selfUpgradeCaptureCandidate = previousCandidate })
	previousExec := selfUpgradeExecImage
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("future attempt must not execute a candidate")
		return nil
	}
	t.Cleanup(func() { selfUpgradeExecImage = previousExec })

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() with future attempt: %v", err)
	}
	if controller.eligible || !strings.Contains(controller.reason, "timestamp is uncertain") {
		t.Fatalf("refreshed future attempt controller = %#v, want unavailable", controller)
	}
}

func TestSelfUpgradeFutureSidecarCannotBeHiddenByPrimaryAttempt(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	now := time.Unix(1_700_000_000, 0).UTC()
	current := selfupgrade.NewAttempt(candidate, now)
	future := selfupgrade.NewAttempt(candidate, now.Add(selfupgrade.AttemptFutureSkew+time.Second))
	state := selfUpgradeStateFile{
		Schema:           selfUpgradeStateSchema,
		Generation:       controller.generation,
		IncumbentVersion: incumbent.EmbeddedVersion,
		IncumbentSHA256:  incumbent.SHA256,
		Attempts:         []selfupgrade.Attempt{current},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.statePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	attemptsRaw, err := json.Marshal(selfUpgradeAttemptsFile{
		Schema:   selfUpgradeAttemptsSchema,
		Attempts: []selfupgrade.Attempt{future},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selfUpgradeAttemptsPath(controller.statePath), append(attemptsRaw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { selfUpgradeNow = previousNow })

	if err := loadSelfUpgradeState(controller); err != nil {
		t.Fatalf("load future sidecar: %v", err)
	}
	if controller.eligible || !strings.Contains(controller.reason, "timestamp is uncertain") {
		t.Fatalf("future sidecar controller = %#v, want unavailable", controller)
	}
	if len(controller.attempts) != 1 || controller.attempts[0] != future {
		t.Fatalf("future sidecar attempts = %#v, want future entry preserved", controller.attempts)
	}
}

func TestSelfUpgradeLedgerRefusesRolledBackFreshCandidate(t *testing.T) {
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
	controller := testSelfUpgradeController(dir, candidateBPath, incumbent)
	now := time.Unix(1_700_000_001, 0)
	controller.attempts = []selfupgrade.Attempt{
		selfupgrade.NewAttempt(candidateB, now.Add(-time.Second)),
		selfupgrade.NewAttempt(candidateC, now.Add(-time.Second)),
	}
	previousNow := selfUpgradeNow
	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	selfUpgradeNow = func() time.Time { return now }
	selfUpgradeCaptureCandidate = func(string) (selfupgrade.ImageEvidence, error) {
		return candidateB, nil
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
	if err := controller.saveState(); err != nil {
		t.Fatalf("save attempt ledger: %v", err)
	}
	err := controller.maintain(context.Background())
	if err == nil || !strings.Contains(err.Error(), controller.attempts[0].RefusalReason()) {
		t.Fatalf("rollback maintain error = %v, want B refusal", err)
	}
	if execCalls != 0 {
		t.Fatalf("rollback exec calls = %d, want no retry", execCalls)
	}
	state := readSelfUpgradeStateForTest(t, controller.statePath)
	if len(state.Attempts) != 2 || state.Attempts[0].Status != selfupgrade.AttemptStatusAttempt ||
		state.Attempts[1].Status != selfupgrade.AttemptStatusAttempt {
		t.Fatalf("rollback attempt ledger = %#v, want both fresh attempts preserved", state.Attempts)
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

func TestSelfUpgradeMarkSettledReadsAuthoritativeState(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	writer := testSelfUpgradeController(dir, candidatePath, incumbent)
	reader := testSelfUpgradeController(dir, candidatePath, incumbent)
	previousNow := selfUpgradeNow
	now := time.Unix(1_700_000_001, 0).UTC()
	selfUpgradeNow = func() time.Time { return now }
	t.Cleanup(func() { selfUpgradeNow = previousNow })
	writer.attempts = []selfupgrade.Attempt{selfupgrade.NewAttempt(incumbent, now.Add(-time.Second))}
	if err := writer.saveState(); err != nil {
		t.Fatalf("publish attempt: %v", err)
	}
	if len(reader.attempts) != 0 {
		t.Fatalf("reader local attempts = %#v, want stale empty view", reader.attempts)
	}
	if err := reader.markSettled(); err != nil {
		t.Fatalf("markSettled() = %v", err)
	}
	state := readSelfUpgradeStateForTest(t, reader.statePath)
	if len(state.Attempts) != 1 || state.Attempts[0].Status != selfupgrade.AttemptStatusSettled {
		t.Fatalf("settled state = %#v, want authoritative attempt settled", state)
	}
}

func TestSelfUpgradeRecordAttemptHonorsAuthoritativeRefusal(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	writer := testSelfUpgradeController(dir, candidatePath, incumbent)
	writer.refused = []selfupgrade.RefusedCandidate{selfupgrade.RefusedCandidateFromEvidence(candidate)}
	if err := writer.saveState(); err != nil {
		t.Fatalf("publish refusal: %v", err)
	}
	reader := testSelfUpgradeController(dir, candidatePath, incumbent)
	if _, err := reader.recordAttempt(candidate); !errors.Is(err, errSelfUpgradeCandidateRefused) {
		t.Fatalf("recordAttempt() error = %v, want authoritative refusal", err)
	}
	state := readSelfUpgradeStateForTest(t, reader.statePath)
	if len(state.Attempts) != 0 {
		t.Fatalf("attempts after authoritative refusal = %#v, want none", state.Attempts)
	}
}

func TestSelfUpgradeRetriesAfterTransientAttemptPublicationFailure(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	if err := controller.saveState(); err != nil {
		t.Fatalf("publish initial state: %v", err)
	}
	previousLock := selfUpgradeAcquireLock
	previousCandidate := selfUpgradeCaptureCandidate
	previousExec := selfUpgradeExecImage
	lockCalls := 0
	selfUpgradeAcquireLock = func(path string) (func() error, error) {
		lockCalls++
		if lockCalls == 2 {
			return nil, errors.New("temporary self-upgrade lock failure")
		}
		return previousLock(path)
	}
	selfUpgradeCaptureCandidate = func(path string) (selfupgrade.ImageEvidence, error) {
		return captureSelfUpgradeTestImage(t, path, "1.1.0"), nil
	}
	execCalls := 0
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		execCalls++
		return nil
	}
	t.Cleanup(func() {
		selfUpgradeAcquireLock = previousLock
		selfUpgradeCaptureCandidate = previousCandidate
		selfUpgradeExecImage = previousExec
	})

	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "temporary self-upgrade lock failure") {
		t.Fatalf("first maintain() error = %v, want transient publication failure", err)
	}
	if controller.lastObservation != nil {
		t.Fatalf("last observation = %#v, want deferred publication not cached", controller.lastObservation)
	}
	selfUpgradeAcquireLock = previousLock
	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("second maintain() error = %v, want retry", err)
	}
	if execCalls != 1 {
		t.Fatalf("exec calls = %d, want retry after transient failure", execCalls)
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

func TestSelfUpgradeExpiredAttemptMatchingCandidateExecs(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "new image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	candidate := captureSelfUpgradeTestImage(t, candidatePath, "1.1.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)
	attempt := selfupgrade.NewAttempt(candidate, time.Unix(1_700_000_000, 0).Add(-selfupgrade.AttemptMaxAge-time.Second))
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

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v", err)
	}
	if execCalls != 1 {
		t.Fatalf("exec calls = %d, want one expired-attempt replacement", execCalls)
	}
	if len(controller.attempts) != 1 || controller.attempts[0].Status != selfupgrade.AttemptStatusAttempt ||
		!controller.attempts[0].Matches(candidate) {
		t.Fatalf("refreshed attempts = %#v, want current candidate", controller.attempts)
	}
}

func TestSelfUpgradeUnsettledAttemptRefusesMatchingImageAtStartup(t *testing.T) {
	testSelfUpgradeStartupAttempt(t, selfupgrade.AttemptStatusAttempt, true)
}

func TestSelfUpgradeSettledAttemptDoesNotRefuseMatchingImageAtStartup(t *testing.T) {
	testSelfUpgradeStartupAttempt(t, selfupgrade.AttemptStatusSettled, false)
}

func testSelfUpgradeStartupAttempt(t *testing.T, status string, wantRefusal bool) {
	t.Helper()
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "running image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	writer := testSelfUpgradeController(dir, filepath.Join(dir, "candidate"), incumbent)
	attempt := selfupgrade.NewAttempt(incumbent, time.Unix(1_700_000_000, 0))
	attempt.Status = status
	writer.attempts = []selfupgrade.Attempt{attempt}
	writer.generation = "old-generation"
	if err := writer.saveState(); err != nil {
		t.Fatalf("save startup state: %v", err)
	}

	reader := testSelfUpgradeController(dir, writer.locator, incumbent)
	reader.generation = "new-generation"
	previousNow := selfUpgradeNow
	selfUpgradeNow = func() time.Time { return time.Unix(1_700_000_001, 0) }
	t.Cleanup(func() { selfUpgradeNow = previousNow })
	if err := loadSelfUpgradeState(reader); err != nil {
		t.Fatalf("load startup state: %v", err)
	}
	if got := reader.startupRefusalReason != ""; got != wantRefusal {
		t.Fatalf("startup diagnostic=%t, want %t; controller=%#v", got, wantRefusal, reader)
	}
	if wantRefusal && reader.startupRefusalReason == "" {
		t.Fatal("startup refusal reason is empty")
	}
	if !wantRefusal && reader.startupRefusalReason != "" {
		t.Fatalf("settled startup refusal reason = %q, want empty", reader.startupRefusalReason)
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
