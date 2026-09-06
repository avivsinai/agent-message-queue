//go:build darwin

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func newDarwinWakeRestartStageRecordForTest(t *testing.T) wakeRestartRecord {
	t.Helper()
	return newDarwinWakeRestartStageRecordForRootTest(t, canonicalWakeRoot(t.TempDir()), "codex")
}

func newDarwinWakeRestartStageRecordForRootTest(t *testing.T, root, agent string) wakeRestartRecord {
	t.Helper()
	if os.Getenv("AMQ_TEST_DARWIN_WAKE_STAGE_STATE") == "" {
		setDarwinWakeRestartStateHomeForTest(t, t.TempDir())
	}
	path := filepath.Join(t.TempDir(), "amq")
	copyTestAMQ(t, path)
	candidate, err := captureWakeImageEvidence(path, "stage-reclaim-test")
	if err != nil {
		t.Fatal(err)
	}
	record := wakeRestartRecord{
		RequestID: "0123456789abcdef0123456789abcdef",
		Root:      root,
		Agent:     agent,
		Candidate: candidate,
	}
	record.StagePath, err = planWakeRestartStageForRecordPlatform(record)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func setDarwinWakeRestartStateHomeForTest(t *testing.T, stateHome string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("AMQ_TEST_DARWIN_WAKE_STAGE_STATE", stateHome)
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
	root := canonicalWakeRoot(t.TempDir())
	const agent = "codex"
	previousRecord := newDarwinWakeRestartStageRecordForRootTest(t, root, agent)
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

	currentRecord := newDarwinWakeRestartStageRecordForRootTest(t, root, agent)
	currentRecord.RequestID = "fedcba9876543210fedcba9876543210"
	currentRecord.StagePath, err = planWakeRestartStageForRecordPlatform(currentRecord)
	if err != nil {
		t.Fatal(err)
	}
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

	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	record := currentRecord
	record.Schema = wakeRestartSchemaV2
	record.Status = wakeRestartPending
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
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
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

func TestDarwinRestartStageUsesMachineLocalStateAndSurvivesCandidateRemoval(t *testing.T) {
	stateHome := t.TempDir()
	setDarwinWakeRestartStateHomeForTest(t, stateHome)
	record := newDarwinWakeRestartStageRecordForTest(t)

	wakeStages, err := darwinWakeRestartStageRoot()
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity, err := resolveTreeIdentityToken(record.Root)
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := sha256.Sum256([]byte(rootIdentity))
	wantStagePath := filepath.Join(
		wakeStages,
		hex.EncodeToString(rootDigest[:]),
		record.Agent,
		record.RequestID,
		filepath.Base(record.Candidate.ExecutionPath),
	)
	if record.StagePath != wantStagePath {
		t.Fatalf("stage path = %q, want %q", record.StagePath, wantStagePath)
	}
	if strings.Contains(record.StagePath, record.Candidate.ExecutionPath) {
		t.Fatalf("stage path remains derived from candidate path: %s", record.StagePath)
	}

	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bound.close() }()
	if bound.evidence.Device != record.Candidate.Device || bound.evidence.Inode != record.Candidate.Inode {
		t.Fatalf("same-filesystem stage was not a hardlink: candidate=%#v stage=%#v", record.Candidate, bound.evidence)
	}
	if err := os.RemoveAll(filepath.Dir(record.Candidate.ExecutionPath)); err != nil {
		t.Fatal(err)
	}
	if err := revalidateBoundWakeRestartImagePlatform(bound); err != nil {
		t.Fatalf("revalidate stage after candidate directory removal: %v", err)
	}
	for path := filepath.Dir(record.StagePath); path != wakeStages; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("managed stage directory %s mode = %v, want directory 0700", path, info.Mode())
		}
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
