//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, agentDir, record)
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
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, fixture.record)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestedAndBoundWakeImageEvidenceAllowsDarwinStagedIdentity(t *testing.T) {
	candidate, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	bound := boundWakeImageEvidenceForTest(candidate)
	if !sameRequestedAndBoundWakeImageEvidence(candidate, bound) {
		t.Fatal("method/path-only bound image delta was rejected")
	}
	ctimeChanged := bound
	ctimeChanged.CTimeNS++
	if got := sameRequestedAndBoundWakeImageEvidence(candidate, ctimeChanged); got != (runtime.GOOS == "darwin") {
		t.Fatalf("ctime exception accepted=%v on %s", got, runtime.GOOS)
	}
	mutations := []struct {
		name             string
		acceptedOnDarwin bool
		mutate           func(*wakeImageEvidenceV1)
	}{
		{name: "device", acceptedOnDarwin: true, mutate: func(value *wakeImageEvidenceV1) { value.Device++ }},
		{name: "inode", acceptedOnDarwin: true, mutate: func(value *wakeImageEvidenceV1) { value.Inode++ }},
		{name: "size", mutate: func(value *wakeImageEvidenceV1) { value.Size++ }},
		{name: "digest", mutate: func(value *wakeImageEvidenceV1) { value.SHA256 = strings.Repeat("0", len(value.SHA256)) }},
		{name: "version", mutate: func(value *wakeImageEvidenceV1) { value.EmbeddedVersion += ".other" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := bound
			test.mutate(&changed)
			want := runtime.GOOS == "darwin" && test.acceptedOnDarwin
			if got := sameRequestedAndBoundWakeImageEvidence(candidate, changed); got != want {
				t.Fatalf("%s delta accepted=%v, want %v", test.name, got, want)
			}
		})
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

func TestWakeRestartJoinsClaimedSuccessorWithoutRenotifying(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)
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

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldSleep := wakeRestartSleep
	preflightCalled := false
	notifyCalled := false
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	sleepCalls := 0
	wakeRestartSleep = func(time.Duration) {
		sleepCalls++
		if sleepCalls != 1 {
			return
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
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartSleep = oldSleep
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err != nil {
		t.Fatalf("join claimed successor: result=%#v err=%v", result, err)
	}
	if result.Status != "restarted" || result.PreviousGeneration != fixture.record.Generation ||
		result.Generation != current.Lock.Generation {
		t.Fatalf("join claimed successor result=%#v", result)
	}
	if preflightCalled || notifyCalled || sleepCalls == 0 {
		t.Fatalf("join claimed successor preflight=%v notify=%v sleeps=%d", preflightCalled, notifyCalled, sleepCalls)
	}
}

func TestWakeRestartCreatorNotifyFailurePreservesAdoptedPendingRecord(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)

	type restartOutcome struct {
		result wakeRestartResult
		err    error
	}

	creatorNotifyStarted := make(chan struct{})
	adopterNotified := make(chan struct{})
	var releaseCreator sync.Once
	var published wakeRestartRecord
	var notifyCalls atomic.Int32
	var preflightCalls atomic.Int32
	var nowCalls atomic.Int32
	creatorNotifyErr := errors.New("injected creator notification failure")

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldNow := wakeRestartNow
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalls.Add(1)
		return nil
	}
	wakeRestartNotify = func(
		_ *wakeAgentDir,
		_ wakeLockInspection,
		record wakeRestartRecord,
	) error {
		switch notifyCalls.Add(1) {
		case 1:
			published = record
			close(creatorNotifyStarted)
			<-adopterNotified
			return creatorNotifyErr
		case 2:
			defer releaseCreator.Do(func() { close(adopterNotified) })
			if record.Schema != wakeRestartSchemaV1 ||
				record.Status != wakeRestartPending ||
				!sameWakeRestartRecord(record, published) {
				return fmt.Errorf("adopter notification record = %#v, want published schema-1 pending record %#v", record, published)
			}
			return nil
		default:
			return errors.New("restart request notified more than twice")
		}
	}
	baseNow := time.Unix(1_700_000_000, 0)
	wakeRestartNow = func() time.Time {
		if nowCalls.Add(1) == 1 {
			return baseNow
		}
		return baseNow.Add(wakeRestartWaitTimeout + time.Second)
	}
	t.Cleanup(func() {
		releaseCreator.Do(func() { close(adopterNotified) })
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartNow = oldNow
	})

	creatorDone := make(chan restartOutcome, 1)
	go func() {
		result, err := requestWakeRestart(fixture.root, fixture.agent)
		creatorDone <- restartOutcome{result: result, err: err}
	}()
	select {
	case <-creatorNotifyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("creator did not publish and reach notification within 5s")
	}

	adopterDone := make(chan restartOutcome, 1)
	go func() {
		result, err := requestWakeRestart(fixture.root, fixture.agent)
		adopterDone <- restartOutcome{result: result, err: err}
	}()

	wait := func(label string, done <-chan restartOutcome) restartOutcome {
		t.Helper()
		select {
		case outcome := <-done:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish within 5s", label)
			return restartOutcome{}
		}
	}
	creator := wait("creator", creatorDone)
	adopter := wait("adopter", adopterDone)
	if !errors.Is(creator.err, creatorNotifyErr) {
		t.Fatalf("creator outcome: result=%#v err=%v, want notification failure", creator.result, creator.err)
	}
	if adopter.err == nil || !strings.Contains(adopter.err.Error(), "did not complete") {
		t.Fatalf("adopter outcome: result=%#v err=%v, want bounded wait timeout", adopter.result, adopter.err)
	}
	if preflightCalls.Load() != 1 || notifyCalls.Load() != 2 {
		t.Fatalf(
			"concurrent creator/adopter preflights=%d notifications=%d, want 1 and 2",
			preflightCalls.Load(),
			notifyCalls.Load(),
		)
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
	if !exists || current.Schema != wakeRestartSchemaV1 ||
		current.Status != wakeRestartPending ||
		!sameWakeRestartRecord(current, published) {
		t.Fatalf("restart record after creator failure = %#v, exists=%v; want unchanged published pending record %#v", current, exists, published)
	}
}

func TestConcurrentWakeRestartCallersJoinSuccessorThroughReadinessCommit(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)

	type restartOutcome struct {
		result wakeRestartResult
		err    error
	}
	type successorState struct {
		inspection wakeLockInspection
		record     wakeRestartRecord
		cleanup    func()
	}

	successorClaimed := make(chan successorState, 1)
	allowReadiness := make(chan struct{})
	readinessCommitted := make(chan struct{})
	caller2Waiting := make(chan struct{})
	var caller2WaitingOnce sync.Once
	var readinessCommittedOnce sync.Once
	var notifyCalls atomic.Int32
	var preflightCalls atomic.Int32

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldSleep := wakeRestartSleep
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalls.Add(1)
		return errors.New("concurrent restart unexpectedly replaced the canonical request")
	}
	wakeRestartNotify = func(
		_ *wakeAgentDir,
		expected wakeLockInspection,
		record wakeRestartRecord,
	) (returnErr error) {
		if notifyCalls.Add(1) != 1 {
			return errors.New("concurrent restart unexpectedly renotified the successor")
		}
		if !sameWakeLockInspection(expected, fixture.lock) ||
			!sameWakeRestartRecord(record, fixture.record) {
			return fmt.Errorf("first restart did not adopt the canonical request")
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
			return err
		}
		keepSuccessor := false
		defer func() {
			if !keepSuccessor {
				cleanup()
			}
		}()

		current := inspectWakeLock(fixture.root, fixture.agent)
		var claimed wakeRestartRecord
		var exists bool
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			var readErr error
			claimed, exists, readErr = readWakeRestartRecordAt(dirfd, fixture.agentDir)
			return readErr
		}); err != nil {
			return err
		}
		if !exists {
			return errors.New("successor claim disappeared after lock rotation")
		}
		keepSuccessor = true
		successorClaimed <- successorState{
			inspection: current,
			record:     claimed,
			cleanup:    cleanup,
		}

		<-allowReadiness
		defer readinessCommittedOnce.Do(func() { close(readinessCommitted) })
		if err := writeWakePreparedFileInDir(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			current,
		); err != nil {
			return err
		}
		return consumeWakeRestartAfterPrepared(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			current,
			wakeResumeBootstrap{
				Schema:     wakeRestartSchemaV1,
				RequestID:  fixture.record.RequestID,
				Generation: fixture.record.Generation,
			},
		)
	}
	wakeRestartSleep = func(time.Duration) {
		caller2WaitingOnce.Do(func() { close(caller2Waiting) })
		<-readinessCommitted
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartSleep = oldSleep
	})

	caller1Done := make(chan restartOutcome, 1)
	go func() {
		result, err := requestWakeRestart(fixture.root, fixture.agent)
		caller1Done <- restartOutcome{result: result, err: err}
	}()

	var successor successorState
	select {
	case successor = <-successorClaimed:
		defer successor.cleanup()
	case outcome := <-caller1Done:
		t.Fatalf("caller 1 exited before successor claim: result=%#v err=%v", outcome.result, outcome.err)
	case <-time.After(5 * time.Second):
		t.Fatal("caller 1 did not claim and rotate to the successor within 5s")
	}
	if successor.inspection.Lock.Generation == fixture.record.Generation ||
		successor.record.Schema != wakeRestartSchemaV2 ||
		successor.record.RequestID != fixture.record.RequestID ||
		successor.record.Generation != fixture.record.Generation ||
		successor.record.SuccessorGeneration != successor.inspection.Lock.Generation {
		t.Fatalf(
			"successor state after claim/rotation: lock=%#v record=%#v",
			successor.inspection,
			successor.record,
		)
	}

	caller2Done := make(chan restartOutcome, 1)
	go func() {
		result, err := requestWakeRestart(fixture.root, fixture.agent)
		caller2Done <- restartOutcome{result: result, err: err}
	}()
	select {
	case <-caller2Waiting:
	case outcome := <-caller2Done:
		t.Fatalf("caller 2 exited instead of joining successor: result=%#v err=%v", outcome.result, outcome.err)
	case <-time.After(5 * time.Second):
		t.Fatal("caller 2 did not join the claimed successor within 5s")
	}

	current := inspectWakeLock(fixture.root, fixture.agent)
	var canonical wakeRestartRecord
	var canonicalExists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var readErr error
		canonical, canonicalExists, readErr = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if !sameWakeLockInspection(successor.inspection, current) || !canonicalExists ||
		!sameWakeRestartRecord(successor.record, canonical) {
		t.Fatalf(
			"caller 2 changed canonical successor state: lock=%#v exists=%v record=%#v",
			current,
			canonicalExists,
			canonical,
		)
	}

	close(allowReadiness)
	await := func(label string, done <-chan restartOutcome) restartOutcome {
		t.Helper()
		select {
		case outcome := <-done:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish within 5s", label)
			return restartOutcome{}
		}
	}
	caller1 := await("caller 1", caller1Done)
	caller2 := await("caller 2", caller2Done)
	for label, outcome := range map[string]restartOutcome{
		"caller 1": caller1,
		"caller 2": caller2,
	} {
		if outcome.err != nil || outcome.result.Status != "restarted" ||
			outcome.result.PreviousGeneration != fixture.record.Generation ||
			outcome.result.Generation != successor.inspection.Lock.Generation {
			t.Fatalf("%s outcome: result=%#v err=%v", label, outcome.result, outcome.err)
		}
	}
	if notifyCalls.Load() != 1 || preflightCalls.Load() != 0 {
		t.Fatalf(
			"concurrent callers notify=%d preflight=%d, want 1 and 0",
			notifyCalls.Load(),
			preflightCalls.Load(),
		)
	}
	current = inspectWakeLock(fixture.root, fixture.agent)
	if !sameWakeLockInspection(successor.inspection, current) {
		t.Fatalf("readiness commit changed successor lock: %#v", current)
	}
	prepared, err := validateWakePreparedFileAgainstInspection(
		fixture.root,
		fixture.agent,
		current,
	)
	if err != nil || !prepared {
		t.Fatalf("successor readiness after concurrent callers: prepared=%v err=%v", prepared, err)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, canonicalExists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if canonicalExists {
		t.Fatal("canonical restart request survived successor readiness commit")
	}
}

func TestWakeRestartAckFailurePreservesClaimedSuccessor(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)

	ackErr := errors.New("injected restart acknowledgement failure")
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		return nil
	}
	var successor wakeLockInspection
	var successorCleanup func()
	wakeRestartNotify = func(
		_ *wakeAgentDir,
		_ wakeLockInspection,
		record wakeRestartRecord,
	) error {
		bound := boundWakeImageEvidenceForTest(record.Candidate)
		if runtime.GOOS == "darwin" {
			bound.ExecutionPath = record.StagePath
			var err error
			record, err = persistWakeRestartBoundImage(record, bound)
			if err != nil {
				return err
			}
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
				Schema:             wakeRestartSchemaV1,
				RequestID:          record.RequestID,
				Generation:         record.Generation,
				BoundImage:         &bound,
				PreviousBoundImage: record.PreviousBoundImage,
			},
		)
		if err != nil {
			return err
		}
		successorCleanup = cleanup
		successor = inspectWakeLock(fixture.root, fixture.agent)
		return ackErr
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		if successorCleanup != nil {
			successorCleanup()
		}
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if !errors.Is(err, ackErr) || !strings.Contains(result.Reason, ackErr.Error()) {
		t.Fatalf("restart result = %#v err=%v, want acknowledgement failure", result, err)
	}
	var claimed wakeRestartRecord
	var exists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var readErr error
		claimed, exists, readErr = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if !exists || claimed.Schema != wakeRestartSchemaV2 ||
		claimed.Status != wakeRestartPending ||
		claimed.SuccessorGeneration != successor.Lock.Generation {
		t.Fatalf("claimed successor was changed by late refusal: exists=%v record=%#v", exists, claimed)
	}
	if err := writeWakePreparedFileInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		successor,
	); err != nil {
		t.Fatal(err)
	}
	if err := consumeWakeRestartAfterPrepared(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		successor,
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  claimed.RequestID,
			Generation: claimed.Generation,
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestGenericCleanupReconcilesPendingRestartOwnership(t *testing.T) {
	t.Run("schema 1 stop wins before successor claim", func(t *testing.T) {
		fixture := newWakeRestartFixture(t)
		if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
			return cleanupGenericWakeGenerationAt(
				dirfd,
				fixture.agentDir,
				fixture.root,
				fixture.agent,
				fixture.lock,
				wakeLockAcquireOptions{wakeMode: wakeInjectModeNone},
			)
		}); err != nil {
			t.Fatal(err)
		}
		if current := inspectWakeLock(fixture.root, fixture.agent); current.Exists {
			t.Fatalf("schema-1 cleanup left wake lock: %#v", current)
		}
		restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
		if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
			t.Fatalf("schema-1 cleanup left canonical restart record: %v", err)
		}
		quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
		if err != nil || len(quarantined) != 1 {
			t.Fatalf("schema-1 cleanup quarantine = %v, err=%v", quarantined, err)
		}
	})

	t.Run("future restart record is quarantined before lock removal", func(t *testing.T) {
		fixture := newWakeRestartFixture(t)
		restartPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
		if err := os.WriteFile(
			restartPath,
			[]byte("{\"schema\":99,\"request_id\":\"future\"}\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
			return cleanupGenericWakeGenerationAt(
				dirfd,
				fixture.agentDir,
				fixture.root,
				fixture.agent,
				fixture.lock,
				wakeLockAcquireOptions{wakeMode: wakeInjectModeNone},
			)
		}); err != nil {
			t.Fatal(err)
		}
		if current := inspectWakeLock(fixture.root, fixture.agent); current.Exists {
			t.Fatalf("future-record cleanup left wake lock: %#v", current)
		}
		if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
			t.Fatalf("future restart record survived canonical cleanup: %v", err)
		}
		quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
		if err != nil || len(quarantined) != 1 {
			t.Fatalf("future restart quarantine = %v, err=%v", quarantined, err)
		}
	})

	t.Run("schema 2 live handoff is preserved", func(t *testing.T) {
		fixture := newWakeRestartFixture(t)
		bound := boundWakeImageEvidenceForTest(fixture.candidate)
		prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)
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
		err = withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
			return cleanupGenericWakeGenerationAt(
				dirfd,
				fixture.agentDir,
				fixture.root,
				fixture.agent,
				current,
				wakeLockAcquireOptions{
					wakeMode:       wakeInjectModeNone,
					requestedOwner: &fixture.owner,
				},
			)
		})
		if err == nil || !strings.Contains(err.Error(), "live wake restart successor handoff") {
			t.Fatalf("schema-2 cleanup error = %v, want handoff preservation", err)
		}
		if observed := inspectWakeLock(fixture.root, fixture.agent); !sameWakeLockGeneration(current, observed) {
			t.Fatalf("schema-2 cleanup changed successor lock: %#v", observed)
		}
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			record, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
			if readErr != nil {
				return readErr
			}
			if !exists || record.Schema != wakeRestartSchemaV2 ||
				record.SuccessorGeneration != current.Lock.Generation {
				return fmt.Errorf("live successor record changed: exists=%v record=%#v", exists, record)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWakeRestartLoopRefusesInputDebtWithoutExec(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	execCalled := false
	oldPreflight := wakeRestartPreflight
	oldExec := wakeRestartExec
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartExec = func(string, []string, []string) error {
		execCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartExec = oldExec
	})

	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		injectMode:         wakeInjectModeNone,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		inputDelivery: wakeInputDeliveryState{
			phase:         wakeInputPrimarySubmitPending,
			acceptedBytes: 1,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)
	if execCalled {
		t.Fatal("restart exec ran with partial input debt")
	}
	record, exists, err := func() (wakeRestartRecord, bool, error) {
		var record wakeRestartRecord
		var exists bool
		err := fixture.agentDir.withFD(func(dirfd int) error {
			var err error
			record, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
			return err
		})
		return record, exists, err
	}()
	if err != nil || !exists {
		t.Fatalf("refusal record = %#v exists=%v err=%v", record, exists, err)
	}
	if record.Status != wakeRestartRefused ||
		!strings.HasPrefix(record.Reason, wakeResumeReasonInputProgress+";") ||
		!strings.Contains(record.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) {
		t.Fatalf("refusal record = %#v", record)
	}
	current := inspectWakeLock(fixture.root, fixture.agent)
	if current.Lock.Generation != fixture.lock.Lock.Generation || !current.IdentityConfirmed {
		t.Fatalf("incumbent changed after refusal: %#v", current)
	}
}

func TestWakeRestartExecFailureLeavesIncumbentLive(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	sentinel := errors.New("test exec failure")
	oldBoundPreflight := wakeRestartBoundPreflight
	oldExec := wakeRestartExec
	wakeRestartBoundPreflight = func(got *wakeRestartBoundImage, argv []string, bootstrap wakeResumeBootstrap) error {
		if got == nil || !sameRequestedAndBoundWakeImageEvidence(fixture.candidate, got.evidence) ||
			!reflect.DeepEqual(argv, os.Args) {
			t.Fatalf("restart preflight got bound=%#v argv=%#v", got, argv)
		}
		if bootstrap.RequestID != fixture.record.RequestID || bootstrap.Generation != fixture.record.Generation {
			t.Fatalf("restart preflight bootstrap=%#v", bootstrap)
		}
		return nil
	}
	wakeRestartExec = func(path string, argv, _ []string) error {
		if path == fixture.candidate.ExecutionPath || !filepath.IsAbs(path) || !reflect.DeepEqual(argv, os.Args) {
			t.Fatalf("restart exec got path=%q argv=%#v", path, argv)
		}
		return sentinel
	}
	t.Cleanup(func() {
		wakeRestartBoundPreflight = oldBoundPreflight
		wakeRestartExec = oldExec
	})

	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		injectMode:         wakeInjectModeNone,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	handleWakeRestartAtLoopBoundary(&cfg, watcher, false, false)

	current := inspectWakeLock(fixture.root, fixture.agent)
	if current.Lock.Generation != fixture.lock.Lock.Generation || !current.IdentityConfirmed {
		t.Fatalf("incumbent changed after exec failure: %#v", current)
	}
	var record wakeRestartRecord
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var exists bool
		var err error
		record, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if !exists && err == nil {
			return errors.New("restart refusal disappeared")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if record.Status != wakeRestartRefused || !strings.Contains(record.Reason, sentinel.Error()) {
		t.Fatalf("exec refusal = %#v", record)
	}
	if strings.Count(record.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) != 1 {
		t.Fatalf("exec refusal remedy = %q", record.Reason)
	}
}

func TestWakeRestartRefusalIncludesOneRunnableCheckRemedy(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	t.Setenv(envWakeOwner, "")

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(result.Reason, "requires the exact coop owner environment") {
		t.Fatalf("ownerless restart result=%#v err=%v", result, err)
	}
	remedy := wakeRestartCheckCommand(fixture.root, fixture.agent)
	if strings.Count(result.Reason, remedy) != 1 {
		t.Fatalf("restart refusal remedy = %q, want exactly one %q", result.Reason, remedy)
	}
}

func TestWakeRestartIncompatibleCandidatePreflightLeavesIncumbentLive(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	legacyPath := filepath.Join(t.TempDir(), "amq")
	const legacyVersion = "0.55.0-legacy-version-only"
	legacy := "#!/bin/sh\n" +
		"if [ \"$1\" = --no-update-check ] && [ \"$2\" = --version ]; then\n" +
		"  printf '%s\\n' '" + legacyVersion + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s\\n' 'unknown wake flag or bootstrap' >&2\n" +
		"exit 2\n"
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := captureWakeImageEvidence(legacyPath, legacyVersion)
	if err != nil {
		t.Fatal(err)
	}

	oldCapture := captureCurrentWakeImageEvidence
	oldNotify := wakeRestartNotify
	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) { return candidate, nil }
	notifyCalled := false
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	t.Cleanup(func() {
		captureCurrentWakeImageEvidence = oldCapture
		wakeRestartNotify = oldNotify
	})

	result, restartErr := requestWakeRestart(fixture.root, fixture.agent)
	if restartErr == nil || !strings.Contains(restartErr.Error(), "candidate preflight failed") {
		t.Fatalf("incompatible restart result=%#v err=%v", result, restartErr)
	}
	if notifyCalled {
		t.Fatal("incompatible candidate reached the incumbent signal boundary")
	}
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !current.IdentityConfirmed || current.Lock.Generation != fixture.lock.Lock.Generation {
		t.Fatalf("incumbent changed after incompatible preflight: %#v", current)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, exists, err := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err == nil && exists {
			return errors.New("incompatible candidate published a restart record")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeRestartRequiresCurrentPlatformTransportBeforePublication(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	legacy := fixture.lock.Lock
	legacy.ResumeSignal = ""
	legacy.ControlSocket = wakeControlSocketPath(fixture.root, fixture.agent, legacy.Generation)
	if legacy.ControlSocket == "" {
		legacy.ControlSocket = filepath.Join(fixture.agentDir.path, ".wake-control-legacy.sock")
	}
	writeWakeLockForTest(t, fixture.root, fixture.agent, legacy)

	preflightCalled := false
	notifyCalled := false
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	notifyErr := errors.New("stop after safe platform notification")
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return notifyErr
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if runtime.GOOS == "darwin" {
		if !errors.Is(err, notifyErr) || !preflightCalled || !notifyCalled {
			t.Fatalf("Darwin control restart result=%#v err=%v preflight=%v notify=%v", result, err, preflightCalled, notifyCalled)
		}
	} else {
		if err == nil || !strings.Contains(err.Error(), "safe restart transport") || preflightCalled || notifyCalled {
			t.Fatalf("Linux control restart result=%#v err=%v preflight=%v notify=%v", result, err, preflightCalled, notifyCalled)
		}
		if _, statErr := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(statErr) {
			t.Fatalf("unsupported transport published restart record: %v", statErr)
		}
	}
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !current.IdentityConfirmed || current.Lock.Generation != legacy.Generation || current.Lock.ResumeSignal != "" {
		t.Fatalf("control-only incumbent changed: %#v", current)
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

func TestWakeRestartAdoptsPendingCurrentGenerationAndRenotifies(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	pending := fixture.record
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, pending)
	}); err != nil {
		t.Fatal(err)
	}

	preflightCalled := false
	notifyCalled := false
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	notifyErr := errors.New("stop after adopted restart notification")
	wakeRestartNotify = func(_ *wakeAgentDir, current wakeLockInspection, record wakeRestartRecord) error {
		notifyCalled = true
		if !sameWakeLockInspection(current, fixture.lock) || !sameWakeRestartRecord(record, pending) {
			t.Fatalf("adopted notification current=%#v record=%#v", current, record)
		}
		return notifyErr
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if !errors.Is(err, notifyErr) {
		t.Fatalf("adopted restart result=%#v err=%v", result, err)
	}
	if preflightCalled || !notifyCalled {
		t.Fatalf("adopted restart preflight=%v notify=%v", preflightCalled, notifyCalled)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || !sameWakeRestartRecord(current, pending) {
			return errors.New("pending restart record changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !current.IdentityConfirmed || current.Lock.Generation != fixture.lock.Lock.Generation {
		t.Fatalf("incumbent changed after second request: %#v", current)
	}
}

func TestWakeRestartPreservesClaimBeforeSuccessorPublication(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	claimed := fixture.record
	claimed.Schema = wakeRestartSchemaV2
	claimed.SuccessorGeneration = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, claimed)
	}); err != nil {
		t.Fatal(err)
	}

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	preflightCalled := false
	notifyCalled := false
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(err.Error(), "is preserved before successor publication") ||
		!strings.Contains(result.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) {
		t.Fatalf("unstable claim result=%#v err=%v", result, err)
	}
	if preflightCalled || notifyCalled {
		t.Fatalf("unstable claim reached preflight=%v notify=%v", preflightCalled, notifyCalled)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || !sameWakeRestartRecord(current, claimed) {
			return fmt.Errorf("unstable claim changed: %#v exists=%v", current, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeRestartPreservesPendingForeignGeneration(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	pending := fixture.record
	pending.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, pending)
	}); err != nil {
		t.Fatal(err)
	}

	preflightCalled := false
	notifyCalled := false
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(err.Error(), "is preserved because it does not match live generation") ||
		!strings.Contains(result.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) {
		t.Fatalf("foreign pending restart result=%#v err=%v", result, err)
	}
	if preflightCalled || notifyCalled {
		t.Fatalf("foreign pending reached preflight=%v notify=%v", preflightCalled, notifyCalled)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || !sameWakeRestartRecord(current, pending) {
			return fmt.Errorf("foreign pending restart record = %#v, exists=%v", current, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeRestartQuarantinesExactInvalidRecordAndStops(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	raw := []byte(`{"schema":`)
	removeWakeRestartRecordForTest(t, fixture)
	path := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	oldNow := wakeQuarantineNow
	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	wakeQuarantineNow = func() time.Time {
		return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	}
	preflightCalled := false
	notifyCalled := false
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeQuarantineNow = oldNow
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, restartErr := requestWakeRestart(fixture.root, fixture.agent)
	if restartErr == nil || !strings.Contains(restartErr.Error(), "was preserved as .wake.restart.quarantined.") ||
		!strings.Contains(result.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) {
		t.Fatalf("invalid restart result=%#v err=%v", result, restartErr)
	}
	if preflightCalled || notifyCalled {
		t.Fatalf("invalid restart reached preflight=%v notify=%v", preflightCalled, notifyCalled)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("invalid restart source remains: %v", statErr)
	}
	assertExactWakeQuarantineForTest(
		t,
		fixture.agentDir.path,
		wakeRestartFileName+".quarantined.",
		raw,
		before,
	)
}

func TestWakeRestartPreservesExactFutureSchemaRecordAndStops(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	future := fixture.record
	future.Schema = wakeRestartSchemaV2 + 1
	encoded, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["request_id"] = map[string]any{"value": future.RequestID}
	document["future_extension"] = map[string]any{"mode": "new"}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	removeWakeRestartRecordForTest(t, fixture)
	path := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	preflightCalled := false
	notifyCalled := false
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error {
		notifyCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
	})

	result, restartErr := requestWakeRestart(fixture.root, fixture.agent)
	if restartErr == nil || !errors.Is(restartErr, errWakeRestartSchemaTooNew) ||
		!strings.Contains(restartErr.Error(), "future-schema wake restart request is preserved") ||
		!strings.Contains(restartErr.Error(), "newer AMQ") ||
		!strings.Contains(result.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) {
		t.Fatalf("future restart result=%#v err=%v", result, restartErr)
	}
	if preflightCalled || notifyCalled {
		t.Fatalf("future restart reached preflight=%v notify=%v", preflightCalled, notifyCalled)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("future restart record identity changed")
	}
	afterRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, afterRaw) {
		t.Fatal("future restart record content changed")
	}
	quarantined, err := filepath.Glob(path + ".quarantined.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("future restart record was quarantined: %v", quarantined)
	}
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

func TestWakeRestartStraySignalWithoutRecordIsNoOp(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	before := snapshotWakeCheckTree(t, fixture.root)

	bindCalled := false
	execCalled := false
	oldBind := wakeRestartBind
	oldExec := wakeRestartExec
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalled = true
		return nil, errors.New("unexpected bind")
	}
	wakeRestartExec = func(string, []string, []string) error {
		execCalled = true
		return errors.New("unexpected exec")
	}
	t.Cleanup(func() {
		wakeRestartBind = oldBind
		wakeRestartExec = oldExec
	})
	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		injectMode:         wakeInjectModeNone,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
	}
	handleWakeRestartAtLoopBoundary(
		&cfg,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	)
	if bindCalled || execCalled {
		t.Fatalf("stray signal reached bind=%v exec=%v", bindCalled, execCalled)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestValidateWakeRestartArgvRejectsRepairAndArbitraryInjection(t *testing.T) {
	root := secureTempDirForTest(t)
	base := []string{"amq", "wake", "--root", root, "--me", "codex"}
	if err := validateWakeRestartArgv(base, root, "codex"); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--repair-lineage=old", "--inject-via=/bin/echo", "--inject-cmd=true", "--interrupt-cmd=ctrl-c"} {
		argv := append(append([]string(nil), base...), flag)
		if err := validateWakeRestartArgv(argv, root, "codex"); err == nil {
			t.Fatalf("argv %v was accepted", argv)
		}
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

func TestWakeCheckRestartRequiresMatchingOwnerAndPreparedProof(t *testing.T) {
	t.Run("missing prepared proof", func(t *testing.T) {
		fixture := newWakeRestartFixture(t)
		removeWakeRestartRecordForTest(t, fixture)
		decision := buildWakeCheckDecision(
			fixture.root,
			fixture.agent,
			fixture.lock,
			nil,
			false,
		)
		if decision.Reload.Status != wakeReloadUnavailable ||
			decision.Reload.ReasonCode != wakeReloadReasonNotPrepared ||
			decision.RestartCapability != wakeRestartUnavailable ||
			decision.Action.Kind != wakeActionWaitForStableState ||
			decision.Action.Actor != wakeActionActorAgent ||
			decision.Action.ReasonCode != wakeReloadReasonNotPrepared ||
			decision.Action.Command == nil ||
			decision.Action.TerminalRequired ||
			decision.Action.Message != "leave wake state unchanged and retry after restart preparation reaches a stable state" {
			t.Fatalf("unprepared restart decision = %#v", decision)
		}
		wantArgs := []string{
			"wake", "check", "--root", fixture.root, "--me", fixture.agent,
			"--json", "--json-schema=2",
		}
		if !reflect.DeepEqual(decision.Action.Command.Args, wantArgs) {
			t.Fatalf("unprepared retry args = %#v, want %#v", decision.Action.Command.Args, wantArgs)
		}
	})

	t.Run("pending restart", func(t *testing.T) {
		fixture := newWakeRestartFixture(t)
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
		if decision.Reload.Status != wakeReloadUnavailable ||
			decision.Reload.ReasonCode != wakeReloadReasonRestartPending ||
			decision.RestartCapability != wakeRestartUnavailable ||
			decision.Action.Kind != wakeActionWaitForStableState ||
			decision.Action.Actor != wakeActionActorAgent ||
			decision.Action.ReasonCode != wakeReloadReasonRestartPending ||
			decision.Action.Command == nil ||
			decision.Action.TerminalRequired ||
			decision.Action.Message != "leave wake state unchanged and retry after the pending wake restart reaches a stable state" {
			t.Fatalf("pending restart decision = %#v", decision)
		}
		wantArgs := []string{
			"wake", "check", "--root", fixture.root, "--me", fixture.agent,
			"--json", "--json-schema=2",
		}
		if !reflect.DeepEqual(decision.Action.Command.Args, wantArgs) {
			t.Fatalf("pending retry args = %#v, want %#v", decision.Action.Command.Args, wantArgs)
		}
		before := snapshotWakeCheckTree(t, fixture.root)
		if _, err := captureEnvStdout(t, func() error {
			return runWake(decision.Action.Command.Args[1:])
		}); err != nil {
			t.Fatalf("execute pending-restart check action: %v", err)
		}
		assertWakeCheckTreeUnchanged(t, fixture.root, before)

		ops := runOpsChecksWithSchema(fixture.root, "test", false, wakeCheckSchemaV2)
		var doctorDecision *wakeCheckDecision
		for index := range ops.WakeLocks {
			if ops.WakeLocks[index].Agent == fixture.agent {
				doctorDecision = ops.WakeLocks[index].WakeCheckDecision
				break
			}
		}
		if doctorDecision == nil ||
			doctorDecision.Reload.Status != wakeReloadUnavailable ||
			doctorDecision.Reload.ReasonCode != wakeReloadReasonRestartPending ||
			doctorDecision.Action.Kind != wakeActionWaitForStableState ||
			doctorDecision.Action.Command == nil {
			t.Fatalf("doctor pending restart decision = %#v", doctorDecision)
		}
		assertWakeCheckTreeUnchanged(t, fixture.root, before)
	})

	t.Run("missing caller owner", func(t *testing.T) {
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
		t.Setenv(envWakeOwner, "")
		decision := buildWakeCheckDecision(
			fixture.root,
			fixture.agent,
			fixture.lock,
			nil,
			false,
		)
		if decision.Reload.Status != wakeReloadUnavailable ||
			decision.Reload.ReasonCode != wakeReloadReasonOwnerMismatch ||
			decision.RestartCapability != wakeRestartOperatorOnly ||
			decision.Action.Kind != wakeActionPreserveLiveWake {
			t.Fatalf("owner-mismatched restart decision = %#v", decision)
		}
	})
}

func TestWakeRestartQuiescenceValidatesRetainedInboxCapability(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	moved := fixture.inboxDir.path + ".moved"
	if err := os.Rename(fixture.inboxDir.path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.inboxDir.path, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := wakeConfig{
		me:                 fixture.agent,
		root:               fixture.root,
		injectMode:         wakeInjectModeNone,
		wakeOwner:          &fixture.owner,
		terminalGeneration: fixture.lock.Lock.Generation,
		retainedAgent:      fixture.agentDir,
		retainedInbox:      fixture.inboxDir,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
	}
	decision := classifyWakeRestartAtLoopBoundary(
		&cfg,
		fixedWakeAdmissionWatcher{errors: make(chan error)},
		false,
		false,
	)
	if decision.Disposition == wakeResumeProceed || decision.Reason != wakeResumeReasonDirectoriesUnverified {
		t.Fatalf("replaced retained inbox decision = %#v", decision)
	}
}
