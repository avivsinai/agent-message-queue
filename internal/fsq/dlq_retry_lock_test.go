package fsq

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDLQRetryLockProcessHelper(t *testing.T) {
	mode := os.Getenv("AMQ_DLP_LOCK_HELPER")
	if mode == "" {
		return
	}
	rootPath := os.Getenv("AMQ_DLP_ROOT")
	agent := os.Getenv("AMQ_DLP_AGENT")
	filename := os.Getenv("AMQ_DLP_FILENAME")
	ready := os.Getenv("AMQ_DLP_READY")
	release := os.Getenv("AMQ_DLP_RELEASE")
	identity, err := SnapshotDeliveryRoot(rootPath)
	if err != nil {
		os.Exit(2)
	}
	root, err := OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		os.Exit(2)
	}
	defer func() { _ = root.Close() }()
	err = root.WithDLQEnvelopeLock(agent, filename, func(batch *DeliveryRoot) error {
		if writeErr := os.WriteFile(ready, []byte("ready"), 0o600); writeErr != nil {
			return writeErr
		}
		if mode == "crash" {
			os.Exit(0) // Kernel handle-close must release the advisory lock.
		}
		for {
			if _, statErr := os.Stat(release); statErr == nil {
				break
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			time.Sleep(5 * time.Millisecond)
		}
		return retryFromDLQLocked(batch, agent, filename, false)
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRetryFromDLQSerializesAcrossProcessesAndCrashReleases(t *testing.T) {
	const (
		agent    = "alice"
		filename = "process-retry.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(rootPath, agent), filename), []byte("body"), 0o600); err != nil {
		t.Fatalf("write inbox fixture: %v", err)
	}
	root := openDeliveryRootForTest(t, rootPath)
	dlqPath, err := MoveToDLQ(root, agent, filename, "process-retry", "parse_error", "fixture")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	dlqFilename := filepath.Base(dlqPath)

	ready := filepath.Join(rootPath, "child-ready")
	release := filepath.Join(rootPath, "child-release")
	child := dlqRetryLockHelper(t, "retry", rootPath, agent, dlqFilename, ready, release)
	if err := child.Start(); err != nil {
		t.Fatalf("start retry helper: %v", err)
	}
	childWaited := false
	t.Cleanup(func() {
		if childWaited {
			return
		}
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	waitForTestFile(t, ready)
	parentDone := make(chan error, 1)
	go func() { parentDone <- RetryFromDLQ(openDeliveryRootForTest(t, rootPath), agent, dlqFilename, false) }()
	select {
	case err := <-parentDone:
		t.Fatalf("parent retry escaped child process lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release child retry: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("child retry: %v", err)
	}
	childWaited = true
	if err := <-parentDone; err == nil || !strings.Contains(err.Error(), "retry already delivered") {
		t.Fatalf("parent retry result = %v, want already-delivered refusal", err)
	}

	crashReady := filepath.Join(rootPath, "crash-ready")
	crash := dlqRetryLockHelper(t, "crash", rootPath, agent, dlqFilename, crashReady, "")
	if err := crash.Start(); err != nil {
		t.Fatalf("start crash helper: %v", err)
	}
	crashWaited := false
	t.Cleanup(func() {
		if crashWaited {
			return
		}
		_ = crash.Process.Kill()
		_ = crash.Wait()
	})
	waitForTestFile(t, crashReady)
	if err := crash.Wait(); err != nil {
		t.Fatalf("crash helper: %v", err)
	}
	crashWaited = true
	// The completed retry remains present, so this proves the post-crash lock
	// is acquirable by reaching the normal already-delivered state.
	if err := RetryFromDLQ(openDeliveryRootForTest(t, rootPath), agent, dlqFilename, false); err == nil || !strings.Contains(err.Error(), "retry already delivered") {
		t.Fatalf("retry after crash-lock owner = %v, want unlocked already-delivered refusal", err)
	}
}

func dlqRetryLockHelper(t *testing.T, mode, root, agent, filename, ready, release string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDLQRetryLockProcessHelper")
	cmd.Env = append(os.Environ(),
		"AMQ_DLP_LOCK_HELPER="+mode,
		"AMQ_DLP_ROOT="+root,
		"AMQ_DLP_AGENT="+agent,
		"AMQ_DLP_FILENAME="+filename,
		"AMQ_DLP_READY="+ready,
		"AMQ_DLP_RELEASE="+release,
	)
	return cmd
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper file %s", path)
}

func TestRetryFromDLQSerializesIndependentDeliveryRoots(t *testing.T) {
	const (
		agent    = "alice"
		filename = "concurrent-retry.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(rootPath, agent), filename), []byte("retry body"), 0o600); err != nil {
		t.Fatalf("write inbox fixture: %v", err)
	}
	firstRoot := openDeliveryRootForTest(t, rootPath)
	dlqPath, err := MoveToDLQ(firstRoot, agent, filename, "concurrent-retry", "parse_error", "fixture")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	secondRoot := openDeliveryRootForTest(t, rootPath)
	dlqFilename := filepath.Base(dlqPath)

	firstLocked := make(chan struct{})
	allowFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- firstRoot.WithDLQEnvelopeLock(agent, dlqFilename, func(batch *DeliveryRoot) error {
			close(firstLocked)
			<-allowFirst
			return retryFromDLQLocked(batch, agent, dlqFilename, false)
		})
	}()
	<-firstLocked
	go func() { secondDone <- RetryFromDLQ(secondRoot, agent, dlqFilename, false) }()
	select {
	case err := <-secondDone:
		t.Fatalf("second retry escaped the first retry lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first retry: %v", err)
	}
	if err := <-secondDone; err == nil || !strings.Contains(err.Error(), "retry already delivered") {
		t.Fatalf("second retry error = %v, want clear already-delivered result", err)
	}

	curPath := filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename)
	env, _, err := ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("read retry audit envelope: %v", err)
	}
	if env.RetryCount != 1 || env.RetryPending || !env.RetryDelivered {
		t.Fatalf(
			"retry audit = count:%d pending:%t delivered:%t, want one completed retry",
			env.RetryCount,
			env.RetryPending,
			env.RetryDelivered,
		)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxNew(rootPath, agent), filename)); err != nil {
		t.Fatalf("single retry delivery missing: %v", err)
	}
}

func TestRetryFromDLQCrashRecoveryFinalizesPendingRetryWithVisibleDestination(t *testing.T) {
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "resume-pending.md"
	content := []byte("body")
	dlqPath := createDLQMessage(t, rootPath, "alice", filename, content)
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env.RetryCount = 1
	setRetryState(env, RetryStatePending)
	data, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize pending envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("write pending envelope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(rootPath, "alice"), filename), content, 0o600); err != nil {
		t.Fatalf("write visible retry destination: %v", err)
	}
	if err := RetryFromDLQ(openDeliveryRootForTest(t, rootPath), "alice", filepath.Base(dlqPath), false); !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("resume pending retry = %v, want terminal delivered result", err)
	}
	curPath := filepath.Join(AgentDLQCur(rootPath, "alice"), filepath.Base(dlqPath))
	resumed, _, err := ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("read resumed envelope: %v", err)
	}
	if resumed.RetryCount != 1 || resumed.RetryPending || !resumed.RetryDelivered {
		t.Fatalf(
			"resumed retry = count:%d pending:%t delivered:%t, want count 1 terminal",
			resumed.RetryCount,
			resumed.RetryPending,
			resumed.RetryDelivered,
		)
	}
}

func TestRetryFromDLQCrashRecoveryRefusesPendingRetryWithoutDestination(t *testing.T) {
	const (
		agent    = "alice"
		filename = "missing-pending.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dlqPath := createDLQMessage(t, rootPath, agent, filename, []byte("body"))
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env.RetryCount = 1
	setRetryState(env, RetryStatePending)
	before, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize pending envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, before, 0o600); err != nil {
		t.Fatalf("write pending envelope: %v", err)
	}

	err = RetryFromDLQ(openDeliveryRootForTest(t, rootPath), agent, filepath.Base(dlqPath), true)
	if !errors.Is(err, ErrDLQRetryIndeterminate) {
		t.Fatalf("pending retry without destination = %v, want indeterminate refusal", err)
	}
	after, readErr := os.ReadFile(dlqPath)
	if readErr != nil {
		t.Fatalf("read untouched pending envelope: %v", readErr)
	}
	if string(after) != string(before) {
		t.Fatal("indeterminate retry mutated its pending audit")
	}
	if _, statErr := os.Stat(filepath.Join(AgentDLQCur(rootPath, agent), filepath.Base(dlqPath))); !os.IsNotExist(statErr) {
		t.Fatalf("indeterminate retry moved its envelope: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(rootPath, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("indeterminate retry redelivered original: %v", statErr)
	}
}

func TestRetryFromDLQPendingFinalizeFailureIsNotTerminal(t *testing.T) {
	const (
		agent    = "alice"
		filename = "pending-finalize-failure.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dlqPath := createDLQMessage(t, rootPath, agent, filename, []byte("body"))
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env.RetryCount = 1
	setRetryState(env, RetryStatePending)
	pending, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize pending envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, pending, 0o600); err != nil {
		t.Fatalf("write pending envelope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(rootPath, agent), filename), body, 0o600); err != nil {
		t.Fatalf("write visible retry destination: %v", err)
	}

	deliveryRoot := openDeliveryRootForTest(t, rootPath)
	injected := errors.New("injected recovered-audit sync failure")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == filepath.Join("agents", agent, "dlq", "cur") {
			return injected
		}
		return deliveryRoot.syncDirPlatform(dir)
	}
	err = RetryFromDLQ(deliveryRoot, agent, filepath.Base(dlqPath), false)
	if !errors.Is(err, injected) {
		t.Fatalf("pending finalize failure = %v, want injected durability error", err)
	}
	if errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("pending finalize failure = %v, must not masquerade as clean terminal delivery", err)
	}
}

func TestRetryFromDLQFinalAuditFailureReportsCommittedInboxDelivery(t *testing.T) {
	const (
		agent    = "alice"
		filename = "final-audit-failure.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	content := []byte("retry body")
	dlqPath := createDLQMessage(t, rootPath, agent, filename, content)
	dlqFilename := filepath.Base(dlqPath)
	root := openDeliveryRootForTest(t, rootPath)

	injectedErr := errors.New("injected final retry audit sync failure")
	curDir := filepath.Join("agents", agent, "dlq", BoxCur)
	curSyncs := 0
	root.syncDirForTest = func(dir string) error {
		if dir == curDir {
			curSyncs++
			// The initial pending transition syncs cur three times. The final
			// audit's post-rename sync is the fifth cur sync.
			if curSyncs == 5 {
				return injectedErr
			}
		}
		return root.syncDirPlatform(dir)
	}

	err := RetryFromDLQ(root, agent, dlqFilename, false)
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, injectedErr) {
		t.Fatalf("RetryFromDLQ error = %T %v, want committed inbox result with final-audit cause", err, err)
	}
	wantInbox := filepath.Join(AgentInboxNew(rootPath, agent), filename)
	if committed.FinalPath != wantInbox || committed.Recipient != agent {
		t.Fatalf("committed retry metadata = %#v, want inbox result %q", committed, wantInbox)
	}
	got, readErr := os.ReadFile(wantInbox)
	if readErr != nil || string(got) != string(content) {
		t.Fatalf("retried inbox copy = %q, err=%v", got, readErr)
	}
	env, _, readErr := ReadDLQEnvelopePath(filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename))
	if readErr != nil {
		t.Fatalf("read committed retry audit: %v", readErr)
	}
	if env.RetryCount != 1 || env.RetryPending || !env.RetryDelivered {
		t.Fatalf(
			"committed retry audit = count:%d pending:%t delivered:%t, want completed count 1",
			env.RetryCount,
			env.RetryPending,
			env.RetryDelivered,
		)
	}
}

func TestMoveDLQNewToCurCannotOverwriteConcurrentRetryAudit(t *testing.T) {
	const (
		agent    = "alice"
		filename = "read-race.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(rootPath, agent), filename), []byte("body"), 0o600); err != nil {
		t.Fatalf("write inbox fixture: %v", err)
	}
	firstRoot := openDeliveryRootForTest(t, rootPath)
	dlqPath, err := MoveToDLQ(firstRoot, agent, filename, "read-race", "parse_error", "fixture")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	dlqFilename := filepath.Base(dlqPath)
	secondRoot := openDeliveryRootForTest(t, rootPath)

	firstLocked := make(chan struct{})
	allowFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	readDone := make(chan error, 1)
	go func() {
		firstDone <- firstRoot.WithDLQEnvelopeLock(agent, dlqFilename, func(batch *DeliveryRoot) error {
			close(firstLocked)
			<-allowFirst
			return retryFromDLQLocked(batch, agent, dlqFilename, false)
		})
	}()
	<-firstLocked
	go func() { readDone <- MoveDLQNewToCur(secondRoot, agent, dlqFilename) }()
	select {
	case err := <-readDone:
		t.Fatalf("DLQ read move escaped retry lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := <-readDone; !os.IsNotExist(err) {
		t.Fatalf("late DLQ read move = %v, want missing old new envelope", err)
	}
	env, _, err := ReadDLQEnvelopePath(filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename))
	if err != nil {
		t.Fatalf("read retry audit: %v", err)
	}
	if env.RetryCount != 1 || env.RetryPending || !env.RetryDelivered {
		t.Fatalf(
			"retry audit after read race = count:%d pending:%t delivered:%t",
			env.RetryCount,
			env.RetryPending,
			env.RetryDelivered,
		)
	}
}

func TestInspectDLQEnvelopeWaitsForRetryAndReturnsCurrentAudit(t *testing.T) {
	const (
		agent    = "alice"
		filename = "inspect-after-retry.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	content := []byte("body")
	dlqPath := createDLQMessage(t, rootPath, agent, filename, content)
	dlqFilename := filepath.Base(dlqPath)
	retryRoot := openDeliveryRootForTest(t, rootPath)
	inspectRoot := openDeliveryRootForTest(t, rootPath)

	retryLocked := make(chan struct{})
	releaseRetry := make(chan struct{})
	retryDone := make(chan error, 1)
	go func() {
		retryDone <- retryRoot.WithDLQEnvelopeLock(agent, dlqFilename, func(batch *DeliveryRoot) error {
			close(retryLocked)
			<-releaseRetry
			return retryFromDLQLocked(batch, agent, dlqFilename, false)
		})
	}()
	<-retryLocked

	type inspectResult struct {
		envelope *DLQEnvelope
		body     []byte
		box      string
		err      error
	}
	inspectDone := make(chan inspectResult, 1)
	go func() {
		envelope, body, box, err := InspectDLQEnvelope(inspectRoot, agent, dlqFilename)
		inspectDone <- inspectResult{envelope: envelope, body: body, box: box, err: err}
	}()
	select {
	case result := <-inspectDone:
		t.Fatalf("inspection escaped retry lock: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseRetry)
	if err := <-retryDone; err != nil {
		t.Fatalf("retry: %v", err)
	}
	result := <-inspectDone
	if result.err != nil {
		t.Fatalf("inspect current retry audit: %v", result.err)
	}
	if result.box != BoxCur || result.envelope == nil ||
		result.envelope.RetryCount != 1 || result.envelope.RetryPending || !result.envelope.RetryDelivered {
		t.Fatalf("inspection state = box:%q envelope:%#v, want completed cur retry audit", result.box, result.envelope)
	}
	if string(result.body) != string(content) {
		t.Fatalf("inspection body = %q, want %q", result.body, content)
	}
}
