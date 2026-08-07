//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
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
		ResumeSignal:         wakeResumeSignalUSR1,
		RunningImageEvidence: &candidate,
	}
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

func TestRequestedAndBoundWakeImageEvidenceHasOnlyDarwinHardlinkCTimeException(t *testing.T) {
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
		name   string
		mutate func(*wakeImageEvidenceV1)
	}{
		{name: "device", mutate: func(value *wakeImageEvidenceV1) { value.Device++ }},
		{name: "inode", mutate: func(value *wakeImageEvidenceV1) { value.Inode++ }},
		{name: "size", mutate: func(value *wakeImageEvidenceV1) { value.Size++ }},
		{name: "digest", mutate: func(value *wakeImageEvidenceV1) { value.SHA256 = strings.Repeat("0", len(value.SHA256)) }},
		{name: "version", mutate: func(value *wakeImageEvidenceV1) { value.EmbeddedVersion += ".other" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := bound
			test.mutate(&changed)
			if sameRequestedAndBoundWakeImageEvidence(candidate, changed) {
				t.Fatalf("%s delta was accepted", test.name)
			}
		})
	}
}

func TestAcquireWakeLockAfterResumeKeepsRequestUntilNewPreparedProof(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
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
	var restartExists bool
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		_, restartExists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !restartExists {
		t.Fatal("restart request was consumed before successor readiness")
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
	oldSignal := wakeRestartSignal
	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) { return candidate, nil }
	signalCalled := false
	wakeRestartSignal = func(*os.Process) error {
		signalCalled = true
		return nil
	}
	t.Cleanup(func() {
		captureCurrentWakeImageEvidence = oldCapture
		wakeRestartSignal = oldSignal
	})

	result, restartErr := requestWakeRestart(fixture.root, fixture.agent)
	if restartErr == nil || !strings.Contains(restartErr.Error(), "candidate preflight failed") {
		t.Fatalf("incompatible restart result=%#v err=%v", result, restartErr)
	}
	if signalCalled {
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

func TestWakeRestartRejectsControlOnlyIncumbentBeforePublication(t *testing.T) {
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
	signalCalled := false
	oldPreflight := wakeRestartPreflight
	oldSignal := wakeRestartSignal
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartSignal = func(*os.Process) error {
		signalCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartSignal = oldSignal
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(err.Error(), "direct SIGUSR1 restart support") {
		t.Fatalf("control-only restart result=%#v err=%v", result, err)
	}
	if preflightCalled || signalCalled {
		t.Fatalf("control-only incumbent reached preflight=%v signal=%v", preflightCalled, signalCalled)
	}
	if _, statErr := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("control-only incumbent published restart record: %v", statErr)
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
	oldSignal := wakeRestartSignal
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartSignal = func(*os.Process) error {
		signalCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartSignal = oldSignal
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

func TestWakeRestartRejectsPendingCurrentGenerationBeforePreflight(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	pending := fixture.record
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, pending)
	}); err != nil {
		t.Fatal(err)
	}

	preflightCalled := false
	signalCalled := false
	oldPreflight := wakeRestartPreflight
	oldSignal := wakeRestartSignal
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	wakeRestartSignal = func(*os.Process) error {
		signalCalled = true
		return nil
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartSignal = oldSignal
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil || !strings.Contains(err.Error(), "already pending for generation "+pending.Generation) {
		t.Fatalf("second restart result=%#v err=%v", result, err)
	}
	if preflightCalled || signalCalled {
		t.Fatalf("second restart reached preflight=%v signal=%v", preflightCalled, signalCalled)
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || current != pending {
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

func TestWakeRestartReplacesPendingOtherGenerationBeforeSignal(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	pending := fixture.record
	pending.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, pending)
	}); err != nil {
		t.Fatal(err)
	}

	preflightCalled := false
	oldPreflight := wakeRestartPreflight
	oldSignal := wakeRestartSignal
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error {
		preflightCalled = true
		return nil
	}
	signalErr := errors.New("stop after stale request replacement")
	wakeRestartSignal = func(*os.Process) error {
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
			if readErr != nil {
				return readErr
			}
			if !exists || current.Status != wakeRestartPending {
				return fmt.Errorf("replacement restart record = %#v, exists=%v", current, exists)
			}
			if current.Generation != fixture.lock.Lock.Generation || current.RequestID == pending.RequestID {
				return fmt.Errorf("replacement restart record did not supersede stale generation: %#v", current)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
		return signalErr
	}
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartSignal = oldSignal
	})

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if !errors.Is(err, signalErr) {
		t.Fatalf("restart result=%#v err=%v, want signal error", result, err)
	}
	if !preflightCalled {
		t.Fatal("replacement restart did not preflight candidate")
	}
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		current, exists, readErr := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if readErr != nil {
			return readErr
		}
		if !exists || current.Generation != fixture.lock.Lock.Generation ||
			current.RequestID == pending.RequestID || current.Status != wakeRestartRefused ||
			!strings.Contains(current.Reason, signalErr.Error()) ||
			strings.Count(current.Reason, wakeRestartCheckCommand(fixture.root, fixture.agent)) != 1 {
			return fmt.Errorf("post-signal replacement restart record = %#v, exists=%v", current, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeRestartClientWaitsForPreparedRequestConsumption(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	const nextGeneration = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	oldPreflight := wakeRestartPreflight
	oldSignal := wakeRestartSignal
	oldSleep := wakeRestartSleep
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartSignal = func(*os.Process) error {
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
		wakeRestartSignal = oldSignal
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
	wakeRestartBind = func(wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
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
