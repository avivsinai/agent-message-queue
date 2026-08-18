//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func newDarwinWakeRestartStageRecordForTest(t *testing.T) wakeRestartRecord {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := captureWakeImageEvidence(path, "stage-reclaim-test")
	if err != nil {
		t.Fatal(err)
	}
	record := wakeRestartRecord{
		RequestID: "0123456789abcdef0123456789abcdef",
		Candidate: candidate,
	}
	record.StagePath, err = planWakeRestartStagePlatform(candidate, record.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type consecutiveDarwinWakeRestartFixture struct {
	root             string
	agent            string
	agentDir         *wakeAgentDir
	record           wakeRestartRecord
	previousEvidence wakeImageEvidenceV1
	currentEvidence  wakeImageEvidenceV1
	lockPath         string
	stale            wakeLockInspection
}

func newConsecutiveDarwinWakeRestartFixture(t *testing.T) consecutiveDarwinWakeRestartFixture {
	t.Helper()
	previousRecord := newDarwinWakeRestartStageRecordForTest(t)
	previousBound, err := bindWakeRestartCandidateAtPlatform(
		previousRecord.Candidate,
		previousRecord.StagePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	previousEvidence := previousBound.evidence
	if err := previousBound.file.Close(); err != nil {
		t.Fatal(err)
	}
	previousBound.file = nil

	currentRecord := newDarwinWakeRestartStageRecordForTest(t)
	currentBound, err := bindWakeRestartCandidateAtPlatform(
		currentRecord.Candidate,
		currentRecord.StagePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	currentEvidence := currentBound.evidence
	if err := currentBound.file.Close(); err != nil {
		t.Fatal(err)
	}
	currentBound.file = nil

	root := canonicalWakeRoot(t.TempDir())
	const agent = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	record := currentRecord
	record.Schema = wakeRestartSchemaV2
	record.Status = wakeRestartPending
	record.Root = root
	record.Agent = agent
	record.Generation = "abcdef0123456789abcdef0123456789"
	record.SuccessorGeneration = "fedcba9876543210fedcba9876543210"
	record.Owner = validWakeResumeOwnerForTest()
	record.BoundImage = &currentEvidence
	record.PreviousBoundImage = &previousEvidence
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	if err := agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
		PID:                  4242,
		Executable:           "/opt/homebrew/bin/amq",
		Generation:           record.SuccessorGeneration,
		RunningImageEvidence: &currentEvidence,
	})
	currentProcess := inspectWakeProcess(os.Getpid())
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == os.Getpid() {
			return currentProcess
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})
	stale := inspectWakeLock(root, agent)
	if stale.Status != wakeLockStale {
		t.Fatalf("wake status = %s, want stale", stale.Status)
	}
	return consecutiveDarwinWakeRestartFixture{
		root:             root,
		agent:            agent,
		agentDir:         agentDir,
		record:           record,
		previousEvidence: previousEvidence,
		currentEvidence:  currentEvidence,
		lockPath:         lockPath,
		stale:            stale,
	}
}

func TestDarwinReclaimsCrashStageFromPersistedIntentBeforeBoundEvidence(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	// Model a hard crash after the hardlink exists but before BoundImage is
	// persisted: release the test descriptor without running normal cleanup.
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil

	if err := reclaimWakeRestartStagePlatform(record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(record.StagePath); !os.IsNotExist(err) {
		t.Fatalf("crash stage survived exact intent-based reclaim: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(record.StagePath)); !os.IsNotExist(err) {
		t.Fatalf("crash stage directory survived exact reclaim: %v", err)
	}
}

func TestDarwinCrashStageReclaimPreservesReplacement(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil
	if err := os.Remove(record.StagePath); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile("/usr/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record.StagePath, replacement, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := reclaimWakeRestartStagePlatform(record); err == nil {
		t.Fatal("reclaim accepted a replacement stage")
	}
	if _, err := os.Lstat(record.StagePath); err != nil {
		t.Fatalf("replacement stage was removed: %v", err)
	}
}

func TestPrepareCoopWakeLockReclaimsStaleDarwinStageAfterParentDisappears(t *testing.T) {
	for _, test := range []struct {
		name          string
		replaceParent bool
	}{
		{name: "missing parent"},
		{name: "non-directory parent", replaceParent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := newDarwinWakeRestartStageRecordForTest(t)
			bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
			if err != nil {
				t.Fatal(err)
			}
			evidence := bound.evidence
			if err := bound.file.Close(); err != nil {
				t.Fatal(err)
			}
			bound.file = nil

			root := secureTempDirForTest(t)
			const agent = "codex"
			lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
				PID:                  66121,
				Executable:           "/opt/homebrew/bin/amq",
				Generation:           "removed-cellar-stage",
				RunningImageEvidence: &evidence,
			})
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid, Running: false}
			})

			stageParent := filepath.Dir(filepath.Dir(record.StagePath))
			if err := os.RemoveAll(stageParent); err != nil {
				t.Fatal(err)
			}
			if test.replaceParent {
				if err := os.WriteFile(stageParent, []byte("former Cellar directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := prepareCoopWakeLock(root, agent, true, "unused"); err != nil {
				t.Fatalf("coop startup preflight: %v", err)
			}
			if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
				t.Fatalf("stale wake lock survived coop startup preflight: %v", err)
			}

			agentDir, err := openWakeAgentDir(root, agent)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agentDir.Close() }()
			cleanup, err := acquireWakeLockWithOptionsInDir(
				agentDir,
				root,
				agent,
				wakeLockAcquireOptions{wakeMode: wakeInjectModeNone},
			)
			if err != nil {
				t.Fatalf("coop wake acquisition after stale removal: %v", err)
			}
			cleanup()
		})
	}
}

func TestDarwinPersistedStageCleanupRefusesReplacedStageDirectory(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := bound.evidence
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil

	stageDir := filepath.Dir(record.StagePath)
	if err := os.Remove(record.StagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stageDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stageDir, []byte("hostile replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = cleanupPersistedDarwinWakeRestartStage(evidence)
	if err == nil || !strings.Contains(err.Error(), "refuse reclaim of replaced Darwin wake restart stage directory") {
		t.Fatalf("cleanup replaced stage directory = %v, want explicit refusal", err)
	}
	if info, statErr := os.Lstat(stageDir); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("replacement stage directory changed: info=%v err=%v", info, statErr)
	}
}

func TestDoctorDiagnosesCrashOrphanedDarwinRestartStage(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bound.close() }()

	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	evidence := bound.evidence
	inspection := wakeLockInspection{
		Exists: true,
		Status: wakeLockStale,
		Root:   root,
		Agent:  "codex",
		Lock: wakeLock{
			Root:                 root,
			Agent:                "codex",
			RunningImageEvidence: &evidence,
		},
	}
	diagnostic := diagnoseWakeRestartStage(root, "codex", inspection)
	if diagnostic.Status != "orphan" || diagnostic.Path != record.StagePath {
		t.Fatalf("Darwin restart-stage diagnostic = %#v, want exact orphan", diagnostic)
	}
}

func TestDoctorFixRetriesExactPersistedDarwinRestartStageReclaim(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil
	record.Schema = wakeRestartSchemaV1
	record.Status = wakeRestartPending
	record.Root = canonicalWakeRoot(t.TempDir())
	record.Agent = "codex"
	record.Generation = "abcdef0123456789abcdef0123456789"
	record.Owner = validWakeResumeOwnerForTest()
	evidence := bound.evidence
	record.BoundImage = &evidence
	if err := fsq.EnsureRootDirs(record.Root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(record.Root, record.Agent); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(record.Root, record.Agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	lockPath := writeWakeLockForTest(t, record.Root, record.Agent, wakeLock{
		PID:                  4242,
		Executable:           "/opt/homebrew/bin/amq",
		Generation:           record.Generation,
		RunningImageEvidence: &evidence,
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	oldReclaim := reclaimWakeRestartStageForStaleLock
	reclaimErr := errors.New("injected persisted stage reclaim failure")
	reclaimAttempts := 0
	reclaimWakeRestartStageForStaleLock = func(record wakeRestartRecord) error {
		reclaimAttempts++
		if reclaimAttempts == 1 {
			return reclaimErr
		}
		return oldReclaim(record)
	}
	t.Cleanup(func() { reclaimWakeRestartStageForStaleLock = oldReclaim })

	stale := inspectWakeLock(record.Root, record.Agent)
	if stale.Status != wakeLockStale {
		t.Fatalf("wake status = %s, want stale", stale.Status)
	}
	first := opsWakeLock{}
	if err := fixStaleWakeLockForDoctor(record.Root, record.Agent, &stale, &first); !errors.Is(err, reclaimErr) {
		t.Fatalf("first doctor fix error = %v, want %v", err, reclaimErr)
	}
	if first.Removed {
		t.Fatalf("first doctor fix removed stale lock: %#v", first)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("first doctor fix removed stale lock: %v", err)
	}
	if _, err := os.Lstat(record.StagePath); err != nil {
		t.Fatalf("first doctor fix removed retryable stage: %v", err)
	}
	if err := agentDir.withFD(func(dirfd int) error {
		current, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || !sameWakeRestartRecord(current, record) {
			return errors.New("first doctor fix did not preserve the exact restart record")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restartPath := filepath.Join(agentDir.path, wakeRestartFileName)
	quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
	if err != nil || len(quarantined) != 0 {
		t.Fatalf("first doctor fix quarantine = %v, err=%v", quarantined, err)
	}

	stale = inspectWakeLock(record.Root, record.Agent)
	second := opsWakeLock{}
	if err := fixStaleWakeLockForDoctor(record.Root, record.Agent, &stale, &second); err != nil {
		t.Fatal(err)
	}
	if !second.Removed || second.Status != "fixed" {
		t.Fatalf("second doctor fix = %#v, want fixed removal", second)
	}
	if reclaimAttempts != 2 {
		t.Fatalf("restart stage reclaim attempts = %d, want 2", reclaimAttempts)
	}
	if _, err := os.Lstat(record.StagePath); !os.IsNotExist(err) {
		t.Fatalf("second doctor fix left exact crash stage: %v", err)
	}
	if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
		t.Fatalf("second doctor fix left live restart record: %v", err)
	}
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("second doctor fix left stale wake lock: %v", err)
	}
	quarantined, err = filepath.Glob(restartPath + ".quarantined.*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("second doctor fix quarantine = %v, err=%v", quarantined, err)
	}
}

func TestDoctorDiagnosesAndReclaimsConsecutiveDarwinRestartStages(t *testing.T) {
	fixture := newConsecutiveDarwinWakeRestartFixture(t)

	live := fixture.stale
	live.Status = wakeLockValid
	live.IdentityConfirmed = true
	live.Process.Running = true
	diagnostic := diagnoseWakeRestartStage(fixture.root, fixture.agent, live)
	if diagnostic.Status != "cleanup-pending" ||
		diagnostic.Path != fixture.previousEvidence.ExecutionPath {
		t.Fatalf("live previous-stage diagnostic = %#v, want exact cleanup-pending stage", diagnostic)
	}
	diagnostic = diagnoseWakeRestartStage(fixture.root, fixture.agent, fixture.stale)
	if diagnostic.Status != "orphan" ||
		diagnostic.Path != fixture.previousEvidence.ExecutionPath {
		t.Fatalf("stale previous-stage diagnostic = %#v, want exact orphan", diagnostic)
	}

	result := opsWakeLock{}
	if err := fixStaleWakeLockForDoctor(
		fixture.root,
		fixture.agent,
		&fixture.stale,
		&result,
	); err != nil {
		t.Fatal(err)
	}
	if !result.Removed || result.Status != "fixed" {
		t.Fatalf("doctor fix = %#v, want fixed removal", result)
	}
	for label, stagePath := range map[string]string{
		"previous": fixture.previousEvidence.ExecutionPath,
		"current":  fixture.currentEvidence.ExecutionPath,
	} {
		if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
			t.Fatalf("%s crash stage survived doctor reclaim: %v", label, err)
		}
		if _, err := os.Lstat(filepath.Dir(stagePath)); !os.IsNotExist(err) {
			t.Fatalf("%s crash stage directory survived doctor reclaim: %v", label, err)
		}
	}
	if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale successor lock survived doctor fix: %v", err)
	}
	restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
		t.Fatalf("live restart record survived doctor fix: %v", err)
	}
	quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("doctor fix quarantine = %v, err=%v", quarantined, err)
	}
}

func TestNextWakeStartupReclaimsRetainedConsecutiveDarwinRestartStages(t *testing.T) {
	for _, test := range []struct {
		name        string
		prepareCoop bool
	}{
		{name: "ordinary wake acquire"},
		{name: "coop startup preflight", prepareCoop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConsecutiveDarwinWakeRestartFixture(t)
			oldGeneration := fixture.record.SuccessorGeneration
			if test.prepareCoop {
				if err := prepareCoopWakeLock(fixture.root, fixture.agent, true, "unused"); err != nil {
					t.Fatal(err)
				}
			}
			cleanup, err := acquireWakeLockWithOptionsInDir(
				fixture.agentDir,
				fixture.root,
				fixture.agent,
				wakeLockAcquireOptions{wakeMode: wakeInjectModeNone},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			current := inspectWakeLock(fixture.root, fixture.agent)
			if !current.Exists || current.Lock.Generation == "" ||
				current.Lock.Generation == oldGeneration {
				t.Fatalf("next wake generation = %#v, want a fresh lock", current)
			}
			for label, stagePath := range map[string]string{
				"previous": fixture.previousEvidence.ExecutionPath,
				"current":  fixture.currentEvidence.ExecutionPath,
			} {
				if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
					t.Fatalf("%s restart stage survived next startup: %v", label, err)
				}
				if _, err := os.Lstat(filepath.Dir(stagePath)); !os.IsNotExist(err) {
					t.Fatalf("%s restart stage directory survived next startup: %v", label, err)
				}
			}
			restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
			if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
				t.Fatalf("live restart record survived next startup: %v", err)
			}
			quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
			if err != nil || len(quarantined) != 1 {
				t.Fatalf("next startup restart quarantine = %v, err=%v", quarantined, err)
			}
		})
	}
}

func TestForeignDarwinRestartRecordDoesNotDeadEndLockRemoval(t *testing.T) {
	for _, action := range []string{"next-start", "doctor"} {
		t.Run(action, func(t *testing.T) {
			fixture := newConsecutiveDarwinWakeRestartFixture(t)
			foreign := fixture.record
			foreign.Generation = "11111111111111111111111111111111"
			foreign.SuccessorGeneration = "22222222222222222222222222222222"
			if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
				return writeWakeRestartRecordAt(dirfd, fixture.agentDir, foreign)
			}); err != nil {
				t.Fatal(err)
			}

			switch action {
			case "next-start":
				cleanup, err := acquireWakeLockWithOptionsInDir(
					fixture.agentDir,
					fixture.root,
					fixture.agent,
					wakeLockAcquireOptions{wakeMode: wakeInjectModeNone},
				)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
			case "doctor":
				result := opsWakeLock{}
				if err := fixStaleWakeLockForDoctor(
					fixture.root,
					fixture.agent,
					&fixture.stale,
					&result,
				); err != nil {
					t.Fatal(err)
				}
				if !result.Removed {
					t.Fatalf("doctor did not remove stale foreign-record lock: %#v", result)
				}
			}

			for label, stagePath := range map[string]string{
				"previous": fixture.previousEvidence.ExecutionPath,
				"current":  fixture.currentEvidence.ExecutionPath,
			} {
				if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
					t.Fatalf("%s foreign restart stage survived %s: %v", label, action, err)
				}
			}
			restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
			if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
				t.Fatalf("foreign canonical restart record survived %s: %v", action, err)
			}
			quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
			if err != nil || len(quarantined) != 1 {
				t.Fatalf("foreign restart quarantine after %s = %v, err=%v", action, quarantined, err)
			}
		})
	}
}

func TestDarwinPreviousCleanupFailurePreservesRecordAndSuccessorLock(t *testing.T) {
	fixture := newConsecutiveDarwinWakeRestartFixture(t)
	cleanupErr := errors.New("injected previous-stage cleanup failure")
	retainLock := false
	bootstrap := wakeResumeBootstrap{
		Schema:             wakeRestartSchemaV1,
		RequestID:          fixture.record.RequestID,
		Generation:         fixture.record.Generation,
		BoundImage:         &fixture.currentEvidence,
		PreviousBoundImage: &fixture.previousEvidence,
	}
	err := commitWakeRestartReadiness(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.stale,
		bootstrap,
		&retainLock,
		func(got wakeResumeBootstrap) error {
			if !sameOptionalWakeImageEvidence(got.PreviousBoundImage, bootstrap.PreviousBoundImage) {
				t.Fatal("readiness cleanup lost exact previous-stage ownership")
			}
			return cleanupErr
		},
	)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("readiness commit error = %v, want %v", err, cleanupErr)
	}
	if !retainLock {
		t.Fatal("previous-stage cleanup failure did not retain the successor lock")
	}
	var current wakeRestartRecord
	var exists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var readErr error
		current, exists, readErr = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if !exists || !sameWakeRestartRecord(current, fixture.record) {
		t.Fatalf("cleanup failure changed restart ownership: exists=%v record=%#v", exists, current)
	}

	lockCleaned := false
	if err := cleanupWakeResumeStageBeforeLock(
		func() error { return cleanupWakeResumeBoundImage(bootstrap) },
		func() { lockCleaned = true },
		&retainLock,
	); err != nil {
		t.Fatal(err)
	}
	if lockCleaned {
		t.Fatal("successful current-stage cleanup removed lock after previous-stage refusal")
	}
	if _, err := os.Lstat(fixture.currentEvidence.ExecutionPath); !os.IsNotExist(err) {
		t.Fatalf("current restart stage survived final cleanup: %v", err)
	}
	if _, err := os.Lstat(fixture.previousEvidence.ExecutionPath); err != nil {
		t.Fatalf("failed previous restart stage was not preserved: %v", err)
	}
	if _, err := os.Lstat(fixture.lockPath); err != nil {
		t.Fatalf("successor lock was not retained for doctor recovery: %v", err)
	}
}

func TestDarwinRestartRetryReclaimsAndQuarantinesRefusedOwnedStage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	refused := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(refused.Candidate, refused.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	boundEvidence := bound.evidence
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil
	refused.Schema = wakeRestartSchemaV1
	refused.Status = wakeRestartRefused
	refused.Reason = "prior cleanup refusal"
	refused.Root = fixture.root
	refused.Agent = fixture.agent
	refused.Generation = fixture.lock.Lock.Generation
	refused.Owner = fixture.owner
	refused.BoundImage = &boundEvidence
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, refused)
	}); err != nil {
		t.Fatal(err)
	}

	retryErr := errors.New("injected retry notification failure")
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		return retryErr
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	if _, err := requestWakeRestart(fixture.root, fixture.agent); !errors.Is(err, retryErr) {
		t.Fatalf("restart retry error = %v, want %v", err, retryErr)
	}
	if _, err := os.Lstat(refused.StagePath); !os.IsNotExist(err) {
		t.Fatalf("refused restart stage survived retry reclaim: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(refused.StagePath)); !os.IsNotExist(err) {
		t.Fatalf("refused restart stage directory survived retry reclaim: %v", err)
	}
	restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("refused retry quarantine = %v, err=%v", quarantined, err)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || current.Status != wakeRestartPending || current.RequestID == refused.RequestID {
			return fmt.Errorf("retry did not preserve an independent pending request: exists=%v record=%#v", exists, current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinFinalStageCleanupPrecedesLockReleaseAndRetainsOnFailure(t *testing.T) {
	cleanupErr := errors.New("injected current-stage cleanup failure")
	retainLock := false
	var events []string
	err := cleanupWakeResumeStageBeforeLock(
		func() error {
			events = append(events, "stage")
			return cleanupErr
		},
		func() { events = append(events, "lock") },
		&retainLock,
	)
	if !errors.Is(err, cleanupErr) || !retainLock {
		t.Fatalf("failed final cleanup: err=%v retain=%v", err, retainLock)
	}
	if len(events) != 1 || events[0] != "stage" {
		t.Fatalf("cleanup order after refusal = %v, want stage only", events)
	}

	retainLock = false
	events = nil
	if err := cleanupWakeResumeStageBeforeLock(
		func() error {
			events = append(events, "stage")
			return nil
		},
		func() { events = append(events, "lock") },
		&retainLock,
	); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "stage" || events[1] != "lock" {
		t.Fatalf("successful cleanup order = %v, want stage then lock", events)
	}
}

func TestDarwinPostAcquireErrorRetainsLockBeforeReadinessCommit(t *testing.T) {
	bootstrap := &wakeResumeBootstrap{Schema: wakeRestartSchemaV1}
	retainLock := newWakeRestartCleanupRetention(bootstrap)
	if !*retainLock {
		t.Fatal("resumed generation did not retain its lock before readiness commit")
	}
	stageCleaned := false
	lockCleaned := false
	if err := cleanupWakeResumeStageBeforeLock(
		func() error {
			stageCleaned = true
			return nil
		},
		func() { lockCleaned = true },
		retainLock,
	); err != nil {
		t.Fatal(err)
	}
	if !stageCleaned {
		t.Fatal("post-acquire failure did not clean the current resume stage")
	}
	if lockCleaned {
		t.Fatal("post-acquire failure removed the lock before readiness commit")
	}
}

func TestDarwinAcquireWakeLockAfterLegacyRestartCompletesMixedVersionHandoff(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	if bound.Method != wakeImageMethodPathnameExecVerified ||
		fixture.record.Schema != wakeRestartSchemaV1 ||
		fixture.record.StagePath != "" || fixture.record.BoundImage != nil {
		t.Fatalf("legacy v0.57.2 restart shape changed: record=%#v bound=%#v", fixture.record, bound)
	}

	cleanup, err := acquireWakeLockAfterResumeInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		wakeLockAcquireOptions{
			wakeMode:            wakeInjectModeNone,
			requestedOwner:      &fixture.owner,
			resumeEligible:      true,
			resumeImageEvidence: &bound,
		},
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  fixture.record.RequestID,
			Generation: fixture.record.Generation,
			BoundImage: &bound,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	current := inspectWakeLock(fixture.root, fixture.agent)
	if !current.IdentityConfirmed || current.PID != fixture.lock.PID ||
		current.Lock.ProcessStart != fixture.lock.Lock.ProcessStart ||
		current.Lock.Generation == fixture.lock.Lock.Generation {
		t.Fatalf("legacy resumed lock = %#v", current)
	}
	var claimed wakeRestartRecord
	var exists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		claimed, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !exists || claimed.Schema != wakeRestartSchemaV2 ||
		claimed.SuccessorGeneration != current.Lock.Generation ||
		claimed.StagePath != "" || claimed.BoundImage != nil {
		t.Fatalf("legacy successor claim = %#v, exists=%v", claimed, exists)
	}
	if err := writeWakePreparedFileInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		current,
	); err != nil {
		t.Fatal(err)
	}
	if err := consumeWakeRestartAfterPrepared(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		current,
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  fixture.record.RequestID,
			Generation: fixture.record.Generation,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("legacy mixed-version handoff was not consumed")
	}
}

func TestDarwinAcquireWakeLockAfterResumeRefusesUnpersistedCurrentStage(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	bound.Method = wakeImageMethodPathnameExecObserved

	cleanup, err := acquireWakeLockAfterResumeInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		wakeLockAcquireOptions{
			wakeMode:            wakeInjectModeNone,
			requestedOwner:      &fixture.owner,
			resumeEligible:      true,
			resumeImageEvidence: &bound,
		},
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  fixture.record.RequestID,
			Generation: fixture.record.Generation,
			BoundImage: &bound,
		},
	)
	if cleanup != nil || err == nil || !strings.Contains(err.Error(), "no persisted bound stage") {
		t.Fatalf("unpersisted current stage cleanup=%v err=%v", cleanup != nil, err)
	}
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !sameWakeLockInspection(fixture.lock, current) {
		t.Fatalf("unpersisted current stage changed incumbent: before=%#v after=%#v", fixture.lock, current)
	}
}
