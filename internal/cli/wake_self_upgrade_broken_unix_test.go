//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wakeSelfUpgradeBrokenCandidateVersion = "9.9.9"

func TestWakeSelfUpgradeBrokenCandidateRefusesOnceThenManualRestartClearsMemory(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidatePath := buildWakeSelfUpgradeBrokenCandidate(t)
	state := selfUpgradeStateForCandidate(t, candidatePath)

	bindCalls := 0
	preflightCalls := 0
	previousBind := wakeRestartBind
	previousBoundPreflight := wakeRestartBoundPreflight
	wakeRestartBind = func(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		return previousBind(record)
	}
	wakeRestartBoundPreflight = func(
		image *wakeRestartBoundImage,
		argv []string,
		bootstrap wakeResumeBootstrap,
	) error {
		preflightCalls++
		return previousBoundPreflight(image, argv, bootstrap)
	}
	t.Cleanup(func() {
		wakeRestartBind = previousBind
		wakeRestartBoundPreflight = previousBoundPreflight
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
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 || preflightCalls != 1 {
		t.Fatalf("broken candidate attempts: bind=%d preflight=%d, want 1 each", bindCalls, preflightCalls)
	}

	refused := readWakeSelfUpgradeRestartRecord(t, fixture)
	if refused.Status != wakeRestartRefused || refused.Source != wakeRestartSourceSelf {
		t.Fatalf("broken candidate record = %#v", refused)
	}
	if !sameWakeSelfUpgradeCandidateIdentity(refused.Candidate, wakeSelfUpgradeCandidateEvidence(t, candidatePath)) {
		t.Fatalf("broken candidate identity = %#v", refused.Candidate)
	}
	identity := wakeSelfUpgradeEvidenceIdentityString(refused.Candidate)
	if !strings.Contains(refused.Reason, identity) {
		t.Fatalf("refusal reason %q does not retain candidate identity %q", refused.Reason, identity)
	}

	for tick := 0; tick < 3; tick++ {
		if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
			t.Fatalf("post-refusal tick %d: %v", tick, err)
		}
	}
	if bindCalls != 1 || preflightCalls != 1 {
		t.Fatalf("refused candidate retried: bind=%d preflight=%d", bindCalls, preflightCalls)
	}
	if got := readWakeSelfUpgradeRestartRecord(t, fixture); got.Status != wakeRestartRefused ||
		!sameWakeSelfUpgradeCandidateIdentity(got.Candidate, refused.Candidate) {
		t.Fatalf("refusal memory changed across ticks: %#v", got)
	}

	quarantined := manuallyRestartAfterBrokenSelfUpgrade(t, fixture)
	if quarantined.Source != wakeRestartSourceSelf || quarantined.Status != wakeRestartRefused ||
		!sameWakeSelfUpgradeCandidateIdentity(quarantined.Candidate, refused.Candidate) {
		t.Fatalf("manual restart did not preserve the exact self refusal in quarantine: %#v", quarantined)
	}
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("manual restart left refusal memory active: %v", err)
	}
}

func buildWakeSelfUpgradeBrokenCandidate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "broken-self-upgrade.go")
	binary := filepath.Join(dir, "amq-broken-self-upgrade")
	program := `package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" ||
		len(args) == 2 && args[0] == "--no-update-check" && args[1] == "--version" {
		fmt.Println("` + wakeSelfUpgradeBrokenCandidateVersion + `")
		return
	}
	for _, arg := range args {
		if arg == "--wake-resume-preflight" {
			fmt.Fprintln(os.Stderr, "intentionally broken wake resume preflight")
			os.Exit(17)
		}
	}
	os.Exit(18)
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, source)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build broken candidate timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build broken candidate: %v\n%s", err, output)
	}
	signTestAMQ(t, binary)
	if err := os.Chmod(binary, 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func wakeSelfUpgradeCandidateEvidence(t *testing.T, path string) wakeImageEvidenceV1 {
	t.Helper()
	evidence, err := captureWakeImageEvidence(path, wakeSelfUpgradeBrokenCandidateVersion)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func readWakeSelfUpgradeRestartRecord(t *testing.T, fixture wakeRestartFixture) wakeRestartRecord {
	t.Helper()
	var record wakeRestartRecord
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var exists bool
		var err error
		record, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("wake restart record is missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func manuallyRestartAfterBrokenSelfUpgrade(t *testing.T, fixture wakeRestartFixture) wakeRestartRecord {
	t.Helper()
	previousCapture := captureCurrentWakeImageEvidence
	previousPreflight := wakeRestartPreflight
	previousNotify := wakeRestartNotify
	previousSleep := wakeRestartSleep
	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) { return fixture.candidate, nil }
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartNotify = func(_ *wakeAgentDir, _ wakeLockInspection, record wakeRestartRecord) error {
		if record.Source == wakeRestartSourceSelf || record.Status != wakeRestartPending {
			return errors.New("manual restart did not replace the self-upgrade refusal")
		}
		restarted := fixture.lock.Lock
		restarted.Generation = "abcdefabcdefabcdefabcdefabcdefab"
		bound := boundWakeImageEvidenceForTest(fixture.candidate)
		restarted.RunningImageEvidence = &bound
		restarted.ImagePath = bound.ExecutionPath
		restarted.ImageVersion = bound.EmbeddedVersion
		writeWakeLockForTest(t, fixture.root, fixture.agent, restarted)
		return writeWakePreparedFileInDir(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			inspectWakeLock(fixture.root, fixture.agent),
		)
	}
	wakeRestartSleep = func(time.Duration) { removeWakeRestartRecordForTest(t, fixture) }
	t.Cleanup(func() {
		captureCurrentWakeImageEvidence = previousCapture
		wakeRestartPreflight = previousPreflight
		wakeRestartNotify = previousNotify
		wakeRestartSleep = previousSleep
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err != nil || result.Status != "restarted" {
		t.Fatalf("manual restart result=%#v err=%v", result, err)
	}
	quarantinePaths, err := filepath.Glob(filepath.Join(fixture.agentDir.path, wakeRestartFileName+".quarantined.*"))
	if err != nil || len(quarantinePaths) != 1 {
		t.Fatalf("manual restart quarantine paths=%v err=%v", quarantinePaths, err)
	}
	raw, err := os.ReadFile(quarantinePaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var quarantined wakeRestartRecord
	if err := json.Unmarshal(raw, &quarantined); err != nil {
		t.Fatal(err)
	}
	return quarantined
}
