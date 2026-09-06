//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinWakeRestartVerifiesBoundStageBeforeExec(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	writeWakeRestartLoopCandidateCopy(t, &fixture)
	fixture.record.Source = wakeRestartSourceForeign
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, fixture.record)
	}); err != nil {
		t.Fatal(err)
	}
	verificationCalls := 0
	execCalls := 0
	sentinel := errors.New("codesign: invalid signature")
	previousVerify := verifyWakeRestartBoundImage
	previousPreflight := wakeRestartBoundPreflight
	previousExec := wakeRestartExec
	previousIgnore := wakeRestartIgnore
	ignoreCalls := 0
	verifyWakeRestartBoundImage = func(bound *wakeRestartBoundImage) error {
		verificationCalls++
		if bound == nil || bound.executionPath == "" {
			t.Fatalf("verified bound image = %#v", bound)
		}
		return sentinel
	}
	wakeRestartBoundPreflight = func(*wakeRestartBoundImage, []string, wakeResumeBootstrap) error {
		return nil
	}
	wakeRestartExec = func(string, []string, []string) error {
		execCalls++
		return errors.New("unexpected exec")
	}
	wakeRestartIgnore = func(...os.Signal) { ignoreCalls++ }
	t.Cleanup(func() {
		verifyWakeRestartBoundImage = previousVerify
		wakeRestartBoundPreflight = previousPreflight
		wakeRestartExec = previousExec
		wakeRestartIgnore = previousIgnore
	})
	runWakeRestartLoopForTest(t, fixture)
	if verificationCalls != 1 {
		t.Fatalf("signature verification calls = %d, want one", verificationCalls)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want zero after signature refusal", execCalls)
	}
	if ignoreCalls != 0 {
		t.Fatalf("signal ignore calls = %d, want verifier to run before ignored window", ignoreCalls)
	}
	record := readRefusedWakeRestartForTest(t, fixture)
	if !strings.Contains(record.Reason, sentinel.Error()) {
		t.Fatalf("refusal reason = %q, want codesign diagnostic", record.Reason)
	}
	if !strings.Contains(record.Reason, errWakeImageRefused.Error()) {
		t.Fatalf("refusal reason = %q, want the image-refused sentinel", record.Reason)
	}
}

func TestDarwinWakeRestartBindingSurvivesPublicPathSwapAndCleansStage(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "amq")
	copyTestAMQ(t, publicPath)
	candidate, err := captureWakeImageEvidence(publicPath, "bound-swap-test")
	if err != nil {
		t.Fatal(err)
	}
	// Link creation is the one intentional ctime mutation in this protocol.
	// Cross a timestamp boundary so the regression proves that the narrow
	// Darwin exception is exercised rather than accidentally unused.
	time.Sleep(1100 * time.Millisecond)
	bound, err := bindWakeRestartCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if bound.evidence.CTimeNS == candidate.CTimeNS {
		_ = bound.close()
		t.Fatal("Darwin hardlink did not change the shared inode ctime")
	}
	if !sameRequestedAndBoundWakeImageEvidence(candidate, bound.evidence) {
		_ = bound.close()
		t.Fatal("Darwin hardlink ctime exception did not preserve exact stable evidence")
	}
	stagePath := bound.executionPath
	stageDir := filepath.Dir(stagePath)

	replacement := filepath.Join(dir, "amq.replacement")
	if err := os.Symlink("/usr/bin/false", replacement); err != nil {
		_ = bound.close()
		t.Fatal(err)
	}
	if err := os.Rename(replacement, publicPath); err != nil {
		_ = bound.close()
		t.Fatal(err)
	}
	if err := exec.Command(publicPath).Run(); err == nil {
		_ = bound.close()
		t.Fatal("normal public path still executed image A after atomic replacement")
	}
	command := exec.Command(stagePath, "-test.run=^TestDarwinWakeRestartBoundPayload$")
	command.Env = setEnvVar(os.Environ(), "AMQ_TEST_WAKE_RESTART_BOUND_EXEC", "payload")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "BOUND_IMAGE_A") {
		_ = bound.close()
		t.Fatalf("execute staged bound image after swap: err=%v output=%q", err, output)
	}
	if err := bound.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("staged image survived exact cleanup: %v", err)
	}
	if _, err := os.Lstat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("staged directory survived exact cleanup: %v", err)
	}
}

// TestDarwinWakeRestartTwoBindersShareOneCandidateInode is the M1 regression:
// two binders staging the SAME brew candidate inode on nearby ticks must both
// succeed. Binder A links its stage; during A's link (before A's cleanup),
// binder B runs a full bind that links and removes a THIRD hardlink on the same
// candidate inode, mutating the shared inode ctime. Before the fix, A's
// post-link ctime-sensitive compare failed and the bind was refused. Now a
// ctime-only difference on the same inode is not an image change, so both binds
// succeed and both bound evidences pass sameRequestedAndBoundWakeImageEvidence.
func TestDarwinWakeRestartTwoBindersShareOneCandidateInode(t *testing.T) {
	dir := t.TempDir()
	candidatePath := filepath.Join(dir, "amq")
	copyTestAMQ(t, candidatePath)
	candidate, err := captureWakeImageEvidence(candidatePath, "two-binder-test")
	if err != nil {
		t.Fatal(err)
	}
	// Cross a timestamp boundary so the ctime mutation is observable.
	time.Sleep(1100 * time.Millisecond)

	originalLink := linkDarwinWakeRestartStage
	binderBRan := false
	t.Cleanup(func() { linkDarwinWakeRestartStage = originalLink })
	// Interpose binder B inside A's link: when A links its stage, run a full
	// independent bind of the same candidate (which links and removes its own
	// stage hardlink on the shared inode, mutating ctime) before A continues.
	linkDarwinWakeRestartStage = func(oldName, newName string) error {
		if err := originalLink(oldName, newName); err != nil {
			return err
		}
		if !binderBRan {
			binderBRan = true
			boundB, err := bindWakeRestartCandidate(candidate)
			if err != nil {
				return fmt.Errorf("concurrent binder B failed: %w", err)
			}
			if !sameRequestedAndBoundWakeImageEvidence(candidate, boundB.evidence) {
				_ = boundB.close()
				return fmt.Errorf("concurrent binder B bound evidence does not match candidate")
			}
			if err := boundB.close(); err != nil {
				return fmt.Errorf("concurrent binder B cleanup: %w", err)
			}
		}
		return nil
	}

	boundA, err := bindWakeRestartCandidate(candidate)
	if err != nil {
		t.Fatalf("binder A failed under concurrent ctime mutation: %v", err)
	}
	defer func() { _ = boundA.close() }()
	if !binderBRan {
		t.Fatal("interposed binder B never ran; test does not exercise the race")
	}
	if !sameRequestedAndBoundWakeImageEvidence(candidate, boundA.evidence) {
		t.Fatalf("binder A bound evidence does not match candidate after concurrent ctime mutation:\ncandidate=%#v\nboundA=%#v", candidate, boundA.evidence)
	}
}

func writeWakeRestartLoopCandidateCopy(t *testing.T, fixture *wakeRestartFixture) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "amq")
	copyTestAMQ(t, path)
	candidate, err := captureWakeImageEvidence(path, fixture.candidate.EmbeddedVersion)
	if err != nil {
		t.Fatal(err)
	}
	fixture.candidate = candidate
	fixture.record.Candidate = candidate
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, fixture.record)
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func runWakeRestartLoopForTest(t *testing.T, fixture wakeRestartFixture) {
	t.Helper()
	cfg := wakeRestartLoopConfigForTest(fixture)
	handleWakeRestartAtLoopBoundary(&cfg, fixedWakeAdmissionWatcher{errors: make(chan error)}, false, false)
}

func wakeRestartLoopConfigForTest(fixture wakeRestartFixture) wakeConfig {
	return wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		injectMode:         wakeInjectModeNone,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		selfUpgrade: wakeSelfUpgradeState{
			Enabled:  true,
			Eligible: true,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
}

func readPendingWakeRestartForTest(t *testing.T, fixture wakeRestartFixture) (wakeRestartRecord, bool, error) {
	t.Helper()
	var record wakeRestartRecord
	var exists bool
	err := fixture.agentDir.withFD(func(dirfd int) error {
		var readErr error
		record, exists, readErr = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return readErr
	})
	return record, exists, err
}

func readRefusedWakeRestartForTest(t *testing.T, fixture wakeRestartFixture) wakeRestartRecord {
	t.Helper()
	record, exists, err := readPendingWakeRestartForTest(t, fixture)
	if err != nil || !exists {
		t.Fatalf("restart record exists=%v err=%v", exists, err)
	}
	if record.Status != wakeRestartRefused {
		t.Fatalf("restart record = %#v; want refused", record)
	}
	return record
}
