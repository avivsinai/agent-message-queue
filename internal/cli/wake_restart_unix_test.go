//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type wakeRestartFixture struct {
	root      string
	agent     string
	owner     wakeOwner
	process   wakeProcessInfo
	candidate wakeImageEvidenceV1
	lock      wakeLockInspection
	record    wakeRestartRecord
	agentDir  *wakeAgentDir
	inboxDir  *wakeInboxDir
}

func newWakeRestartFixture(t *testing.T) wakeRestartFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	ensureCoopWakeMailboxForTest(t, root, agent)
	setCLIVersionForTest(t, "0.56.0-test")
	candidate, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	ownerEnv, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, ownerEnv)
	realProcess := inspectWakeProcess(os.Getpid())
	args := []string{
		candidate.ExecutionPath,
		"wake",
		"--root", root,
		"--me", agent,
		"--inject-mode", wakeInjectModeNone,
		"--interrupt=false",
	}
	process := wakeProcessInfo{
		PID:          os.Getpid(),
		Running:      true,
		StartToken:   realProcess.StartToken,
		BootID:       realProcess.BootID,
		Executable:   "amq",
		Args:         args,
		InspectError: realProcess.InspectError,
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == os.Getpid() {
			return process
		}
		return wakeProcessInfo{PID: pid}
	})
	lock := wakeLock{
		PID:                  os.Getpid(),
		TTY:                  "test-tty",
		Root:                 root,
		Agent:                agent,
		Started:              time.Now().UTC().Format(time.RFC3339),
		ProcessStart:         process.StartToken,
		BootID:               process.BootID,
		Executable:           "amq",
		Args:                 args,
		ImagePath:            candidate.ExecutionPath,
		ImageVersion:         candidate.EmbeddedVersion,
		WakeMode:             wakeInjectModeNone,
		Generation:           "0123456789abcdef0123456789abcdef",
		ResumeSchema:         wakeResumeSchemaV2,
		ResumeOwner:          &owner,
		RunningImageEvidence: &candidate,
	}
	configureWakeRestartAdvertisementPlatform(&lock, root, agent)
	writeWakeLockForTest(t, root, agent, lock)
	inspection := inspectWakeLock(root, agent)
	if !inspection.IdentityConfirmed || inspection.Status != wakeLockValid {
		t.Fatalf("fixture lock = %#v", inspection)
	}
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })
	record := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		RequestID:  "fedcba9876543210fedcba9876543210",
		Status:     wakeRestartPending,
		Root:       root,
		Agent:      agent,
		Generation: lock.Generation,
		Owner:      owner,
		Candidate:  candidate,
	}
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	return wakeRestartFixture{
		root:      root,
		agent:     agent,
		owner:     owner,
		process:   process,
		candidate: candidate,
		lock:      inspection,
		record:    record,
		agentDir:  agentDir,
		inboxDir:  inboxDir,
	}
}

func removeWakeRestartRecordForTest(t *testing.T, fixture wakeRestartFixture) {
	t.Helper()
	err := os.Remove(filepath.Join(fixture.agentDir.path, wakeRestartFileName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func boundWakeImageEvidenceForTest(candidate wakeImageEvidenceV1) wakeImageEvidenceV1 {
	bound := candidate
	if runtime.GOOS == "linux" {
		bound.Method = wakeImageMethodFDExec
		bound.ExecutionPath = "/proc/self/fd/99"
	} else {
		bound.Method = wakeImageMethodPathnameExecVerified
		bound.ExecutionPath = filepath.Join(filepath.Dir(candidate.ExecutionPath), ".amq.amq-restart-test", "amq")
	}
	return bound
}

func prepareWakeRestartRecordForBoundResumeTest(
	t *testing.T,
	fixture *wakeRestartFixture,
	bound *wakeImageEvidenceV1,
) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	stagePath, err := planWakeRestartStagePlatform(fixture.candidate, fixture.record.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	bound.ExecutionPath = stagePath
	fixture.record.StagePath = stagePath
	boundCopy := *bound
	fixture.record.BoundImage = &boundCopy
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, fixture.record)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireWakeLockAfterResumeKeepsRequestUntilNewPreparedProof(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)
	if err := writeWakePreparedFileInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.lock,
	); err != nil {
		t.Fatal(err)
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
		t.Fatalf("resumed lock = %#v", current)
	}
	if current.Lock.RunningImageEvidence == nil || *current.Lock.RunningImageEvidence != bound ||
		current.Lock.ImagePath != bound.ExecutionPath ||
		current.Lock.ImageVersion != bound.EmbeddedVersion {
		t.Fatalf("resumed lock did not publish bound image evidence: %#v", current.Lock)
	}
	prepared, err := validateWakePreparedFileAgainstInspection(
		fixture.root,
		fixture.agent,
		current,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared {
		t.Fatal("old generation prepared marker remained current after resume")
	}
	var restart wakeRestartRecord
	var restartExists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		restart, restartExists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !restartExists {
		t.Fatal("restart request was consumed before successor readiness")
	}
	if restart.Schema != wakeRestartSchemaV2 ||
		restart.Generation != fixture.record.Generation ||
		restart.SuccessorGeneration != current.Lock.Generation {
		t.Fatalf("restart successor claim = %#v", restart)
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
		_, restartExists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if restartExists {
		t.Fatal("ready successor did not consume restart request")
	}
}

func TestWakeRestartRejectsRegisteredOwnerlessInjectViaBeforeSignal(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, fixture.root, fixture.agent, injector, nil)
	if err := writeWakeTarget(fixture.root, fixture.agent, target); err != nil {
		t.Fatal(err)
	}
	ownerless := fixture.lock.Lock
	ownerless.WakeMode = wakeTargetInjectVia
	ownerless.TargetDigest = mustWakeTargetDigest(target)
	ownerless.ControlSocket = wakeControlSocketPath(fixture.root, fixture.agent, ownerless.Generation)
	ownerless.ResumeSchema = 0
	ownerless.ResumeOwner = nil
	ownerless.ResumeSignal = ""
	writeWakeLockForTest(t, fixture.root, fixture.agent, ownerless)

	preflightCalled := false
	signalCalled := false
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		signalCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})
	before := snapshotWakeCheckTree(t, fixture.root)

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(err.Error(), "does not advertise restart support") {
		t.Fatalf("ownerless inject-via restart result=%#v err=%v", result, err)
	}
	if preflightCalled || signalCalled {
		t.Fatalf("ownerless inject-via reached preflight=%v signal=%v", preflightCalled, signalCalled)
	}
	if _, statErr := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("ownerless inject-via published restart record: %v", statErr)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestWakeRestartClientWaitsForPreparedRequestConsumption(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	const nextGeneration = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldSleep := wakeRestartSleep
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		restarted := fixture.lock.Lock
		restarted.Generation = nextGeneration
		bound := boundWakeImageEvidenceForTest(fixture.candidate)
		restarted.RunningImageEvidence = &bound
		restarted.ImagePath = bound.ExecutionPath
		restarted.ImageVersion = bound.EmbeddedVersion
		writeWakeLockForTest(t, fixture.root, fixture.agent, restarted)
		current := inspectWakeLock(fixture.root, fixture.agent)
		if err := writeWakePreparedFileInDir(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			current,
		); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	sleepCalls := 0
	wakeRestartSleep = func(time.Duration) {
		sleepCalls++
		removeWakeRestartRecordForTest(t, fixture)
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartSleep = oldSleep
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err != nil {
		t.Fatalf("restart client: result=%#v err=%v", result, err)
	}
	if result.Status != "restarted" || result.Generation != nextGeneration || sleepCalls == 0 {
		t.Fatalf("restart client returned before request consumption: result=%#v sleep_calls=%d", result, sleepCalls)
	}
}

func TestWakeCheckMakesAdvertisedSelfRestartActionable(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	if err := writeWakePreparedFileInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.lock,
	); err != nil {
		t.Fatal(err)
	}
	decision := buildWakeCheckDecision(
		fixture.root,
		fixture.agent,
		fixture.lock,
		nil,
		false,
	)
	if decision.Reload.Status != wakeReloadReady ||
		decision.Reload.ReasonCode != wakeReloadReasonReady ||
		decision.RestartCapability != wakeRestartAgentSafe ||
		decision.Action.Kind != wakeActionRestartWake ||
		decision.Action.Command == nil {
		t.Fatalf("wake restart decision = %#v", decision)
	}
	wantArgs := []string{
		"wake", "restart", "--root", fixture.root, "--me", fixture.agent,
	}
	if !reflect.DeepEqual(decision.Action.Command.Args, wantArgs) {
		t.Fatalf("restart action args = %#v, want %#v", decision.Action.Command.Args, wantArgs)
	}
}
