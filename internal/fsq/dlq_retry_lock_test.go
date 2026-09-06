package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
