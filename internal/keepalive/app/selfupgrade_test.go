package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeRunVersion = func(string) (string, error) { return "1.1.0", nil }
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

	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeRunVersion = func(string) (string, error) {
		t.Fatal("identical image should not require a version probe")
		return "", nil
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

func TestSelfUpgradeDefersFailedCandidateVersionProbe(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "good image")
	writeExecutableForSelfUpgradeTest(t, candidatePath, "corrupt image")
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	versionCalls := 0
	selfUpgradeRunVersion = func(string) (string, error) {
		versionCalls++
		return "", errors.New("candidate exited unsuccessfully")
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("corrupt candidate must not be executed")
		return nil
	}

	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "deferred for candidate "+candidatePath) {
		t.Fatalf("first maintain() error = %v, want deferred probe error", err)
	}
	if err := controller.maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "deferred for candidate "+candidatePath) {
		t.Fatalf("second maintain() error = %v, want deferred probe error", err)
	}
	if versionCalls != 2 {
		t.Fatalf("version probe calls = %d, want retry after deferred failure", versionCalls)
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

	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	versionCalls := 0
	selfUpgradeRunVersion = func(string) (string, error) {
		versionCalls++
		return "1.1.0", nil
	}
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error { return nil }

	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("first maintain() error = %v", err)
	}
	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("second maintain() error = %v", err)
	}
	if versionCalls != 1 {
		t.Fatalf("version probe calls = %d, want one for one candidate identity", versionCalls)
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

	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeRunVersion = func(string) (string, error) { return "1.1.0", nil }
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
}

func TestSelfUpgradeOptOutDoesNotPublishOrProbe(t *testing.T) {
	dir := t.TempDir()
	controller := &selfUpgradeController{
		enabled:   false,
		eligible:  false,
		statePath: filepath.Join(dir, selfUpgradeStateFileName),
	}
	previousVersion := selfUpgradeRunVersion
	previousExec := selfUpgradeExecImage
	t.Cleanup(func() {
		selfUpgradeRunVersion = previousVersion
		selfUpgradeExecImage = previousExec
	})
	selfUpgradeRunVersion = func(string) (string, error) {
		t.Fatal("opt-out should not probe the candidate")
		return "", nil
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
