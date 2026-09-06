//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
