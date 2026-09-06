package fsq

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a corrupt message in inbox/new
	inboxNew := AgentInboxNew(root, "alice")
	filename := "corrupt_123.md"
	content := []byte("not valid frontmatter at all")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	// Move to DLQ
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "corrupt_123", "parse_error", "missing frontmatter")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	// Verify original removed from inbox/new
	if _, err := os.Stat(filepath.Join(inboxNew, filename)); !os.IsNotExist(err) {
		t.Errorf("original should be removed from inbox/new")
	}

	// Verify DLQ message exists
	if _, err := os.Stat(dlqPath); err != nil {
		t.Errorf("DLQ message should exist: %v", err)
	}

	// Verify DLQ envelope content
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}

	if env.Schema != DLQSchemaVersion {
		t.Errorf("expected schema %s, got %s", DLQSchemaVersion, env.Schema)
	}
	if env.OriginalID != "corrupt_123" {
		t.Errorf("expected original_id corrupt_123, got %s", env.OriginalID)
	}
	if env.OriginalFile != filename {
		t.Errorf("expected original_file %s, got %s", filename, env.OriginalFile)
	}
	if env.FailureReason != "parse_error" {
		t.Errorf("expected failure_reason parse_error, got %s", env.FailureReason)
	}
	if env.RetryCount != 0 {
		t.Errorf("expected retry_count 0, got %d", env.RetryCount)
	}
	if string(body) != string(content) {
		t.Errorf("body mismatch: expected %q, got %q", content, body)
	}
}

func TestMoveClaimedCurToDLQRejectsMismatchedClaimProvenanceBeforeMutation(t *testing.T) {
	const (
		agent    = "alice"
		filename = "claimed-corrupt.md"
	)

	for _, test := range []struct {
		name      string
		recipient string
		finalPath func(root, foreignRoot string) string
	}{
		{
			name:      "wrong recipient",
			recipient: "bob",
			finalPath: func(root, _ string) string {
				return filepath.Join(AgentInboxCur(root, agent), filename)
			},
		},
		{
			name:      "wrong cur path",
			recipient: agent,
			finalPath: func(root, _ string) string {
				return filepath.Join(AgentInboxCur(root, agent), "another-message.md")
			},
		},
		{
			name:      "foreign delivery root",
			recipient: agent,
			finalPath: func(_, foreignRoot string) string {
				return filepath.Join(AgentInboxCur(foreignRoot, agent), filename)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			foreignBase := t.TempDir()
			for _, root := range []string{base, foreignBase} {
				if err := EnsureAgentDirs(root, agent); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
				}
			}

			content := []byte("claimed corrupt payload")
			curPath := filepath.Join(AgentInboxCur(base, agent), filename)
			if err := os.WriteFile(curPath, content, 0o600); err != nil {
				t.Fatalf("write claimed fixture: %v", err)
			}
			claimCause := errors.New("injected committed claim sync failure")
			claimErr := &CommittedDurabilityError{
				FinalPath: test.finalPath(base, foreignBase),
				Recipient: test.recipient,
				Err:       claimCause,
			}

			root := openDeliveryRootForTest(t, base)
			dlqPath, err := MoveClaimedCurToDLQ(
				root,
				agent,
				filename,
				"claimed-corrupt",
				"parse_error",
				"bad payload",
				claimErr,
			)
			if err == nil {
				t.Fatal("mismatched committed claim error = nil, want rejection")
			}
			if dlqPath != "" {
				t.Fatalf("mismatched committed claim DLQ path = %q, want empty", dlqPath)
			}
			if !errors.Is(err, claimCause) {
				t.Fatalf("mismatched committed claim error = %v, want original claim cause", err)
			}
			if got, readErr := os.ReadFile(curPath); readErr != nil || !bytes.Equal(got, content) {
				t.Fatalf("retained cur source = %q, err=%v; want %q", got, readErr, content)
			}
			for _, dir := range []string{
				AgentDLQTmp(base, agent),
				AgentDLQNew(base, agent),
			} {
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatalf("read DLQ directory %s: %v", dir, readErr)
				}
				if len(entries) != 0 {
					t.Fatalf("DLQ directory %s entries = %#v, want none", dir, entries)
				}
			}
		})
	}
}

func TestMoveCurToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	filename := "claimed_corrupt.md"
	content := []byte("not valid frontmatter after claim")
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if err := MoveNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}

	dlqPath, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "claimed_corrupt", "parse_error", "missing frontmatter")
	if err != nil {
		t.Fatalf("MoveCurToDLQ: %v", err)
	}

	if _, err := os.Stat(filepath.Join(AgentInboxCur(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("claimed original should be removed from inbox/cur")
	}

	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}
	if env.SourceDir != BoxCur {
		t.Fatalf("expected source_dir %q, got %q", BoxCur, env.SourceDir)
	}
	if env.OriginalFile != filename {
		t.Fatalf("expected original_file %q, got %q", filename, env.OriginalFile)
	}
	if string(body) != string(content) {
		t.Fatalf("body mismatch: expected %q, got %q", content, body)
	}
}

func TestMoveCurToDLQPostRenameSyncFailureReportsRetainedTransition(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	filename := "indeterminate_dlq.md"
	sourcePath := filepath.Join(AgentInboxCur(root, "alice"), filename)
	if err := os.WriteFile(sourcePath, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	deliveryRoot := openDeliveryRootForTest(t, root)
	dlqNewDir := filepath.Join("agents", "alice", "dlq", "new")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == dlqNewDir {
			return errors.New("injected post-rename sync failure")
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	dlqPath, err := MoveCurToDLQ(deliveryRoot, "alice", filename, "indeterminate_dlq", "parse_error", "bad data")
	if err == nil {
		t.Fatal("MoveCurToDLQ error = nil, want partial transition")
	}
	if dlqPath == "" {
		t.Fatal("MoveCurToDLQ discarded the committed envelope path")
	}
	var transition *DLQTransitionError
	if !errors.As(err, &transition) {
		t.Fatalf("error = %T %v, want typed DLQ transition", err, err)
	}
	if transition.EnvelopePath != dlqPath || transition.SourcePath != sourcePath || !transition.SourceRetained {
		t.Fatalf("transition = (%q,%q,%v), want (%q,%q,true)", transition.EnvelopePath, transition.SourcePath, transition.SourceRetained, dlqPath, sourcePath)
	}
	if _, statErr := os.Stat(dlqPath); statErr != nil {
		t.Fatalf("committed DLQ envelope missing: %v", statErr)
	}
	if _, statErr := os.Stat(sourcePath); statErr != nil {
		t.Fatalf("source was not retained: %v", statErr)
	}
}

func TestRetryFromDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message in inbox/new first
	inboxNew := AgentInboxNew(root, "alice")
	filename := "test_msg.md"
	content := []byte("---\n{\"schema\":1,\"id\":\"test_msg\"}\n---\nHello")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write test msg: %v", err)
	}

	// Move to DLQ
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "test_msg", "test_failure", "test detail")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry from DLQ
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, false); err != nil {
		t.Fatalf("RetryFromDLQ: %v", err)
	}

	// Verify message back in inbox/new
	inboxPath := filepath.Join(inboxNew, filename)
	restoredContent, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("read restored message: %v", err)
	}
	if string(restoredContent) != string(content) {
		t.Errorf("restored content mismatch")
	}

	// Verify DLQ envelope moved to cur with incremented retry count
	dlqCur := AgentDLQCur(root, "alice")
	curPath := filepath.Join(dlqCur, dlqFilename)
	env, _, err := ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope from cur: %v", err)
	}
	if env.RetryCount != 1 {
		t.Errorf("expected retry_count 1 after retry, got %d", env.RetryCount)
	}
	if env.RetryPending || !env.RetryDelivered {
		t.Errorf("completed retry state = pending:%t delivered:%t, want terminal delivery", env.RetryPending, env.RetryDelivered)
	}
}

func TestRetryFromDLQRefusesCompletedEnvelopeAfterDeliveryIsRedLQed(t *testing.T) {
	const (
		agent    = "alice"
		filename = "retry-terminal.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	root := openDeliveryRootForTest(t, rootPath)
	dlqPath := createDLQMessage(t, rootPath, agent, filename, []byte("retry body"))
	dlqFilename := filepath.Base(dlqPath)

	if err := RetryFromDLQ(root, agent, dlqFilename, false); err != nil {
		t.Fatalf("first RetryFromDLQ: %v", err)
	}
	if _, err := MoveToDLQ(root, agent, filename, "retry-terminal-redlq", "parse_error", "still malformed"); err != nil {
		t.Fatalf("consume retried delivery back to DLQ: %v", err)
	}

	err := RetryFromDLQ(root, agent, dlqFilename, true)
	if !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("late retry of completed envelope = %v, want terminal already-delivered refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(rootPath, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("late retry recreated inbox/new delivery: %v", statErr)
	}
	envelope, _, readErr := ReadDLQEnvelopePath(filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename))
	if readErr != nil {
		t.Fatalf("read completed retry audit: %v", readErr)
	}
	if envelope.RetryCount != 1 {
		t.Fatalf("completed retry count = %d, want 1", envelope.RetryCount)
	}
	if envelope.RetryPending || !envelope.RetryDelivered {
		t.Fatalf(
			"completed retry state = pending:%t delivered:%t, want terminal delivery",
			envelope.RetryPending,
			envelope.RetryDelivered,
		)
	}
}

func TestRetryFromDLQLegacyCompletedRetryIsIndeterminateWithoutDestination(t *testing.T) {
	const (
		agent    = "alice"
		filename = "legacy-completed.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Released v1 wrote this same shape both after a successful delivery and
	// after a known pre-commit delivery failure. A positive count alone is
	// therefore ambiguous and must never be retried blindly.
	raw := []byte("---\n" +
		`{"schema":"amq/dlq/v1","id":"legacy-completed","original_id":"legacy-original","original_file":"legacy-completed.md","failure_reason":"parse_error","failure_detail":"legacy fixture","failure_time":"2026-07-28T00:00:00Z","retry_count":1,"source_dir":"new"}` +
		"\n---\nlegacy body")
	dlqPath := filepath.Join(AgentDLQNew(root, agent), "legacy-completed.md")
	if err := os.WriteFile(dlqPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy DLQ envelope: %v", err)
	}

	err := RetryFromDLQ(openDeliveryRootForTest(t, root), agent, filename, true)
	if !errors.Is(err, ErrDLQRetryIndeterminate) {
		t.Fatalf("legacy ambiguous retry = %v, want indeterminate refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(root, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("legacy indeterminate retry recreated inbox/new delivery: %v", statErr)
	}
	if after, readErr := os.ReadFile(dlqPath); readErr != nil || !bytes.Equal(after, raw) {
		t.Fatalf("legacy indeterminate retry mutated envelope: bytes=%q err=%v", after, readErr)
	}
}

func TestRetryFromDLQRejectsTraversalOriginalFile(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "safe_msg.md", []byte("test content"))
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}
	env.OriginalFile = "../escape.md"
	data, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize tampered envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("write tampered envelope: %v", err)
	}

	err = RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected traversal original_file to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid original_file") {
		t.Fatalf("expected invalid original_file error, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "alice", "inbox", "escape.md")); !os.IsNotExist(err) {
		t.Fatalf("retry should not create escaped inbox file, stat err: %v", err)
	}
}

func TestRetryFromDLQMaxRetries(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message and move to DLQ with retry_count = 3
	inboxNew := AgentInboxNew(root, "alice")
	filename := "test_msg.md"
	content := []byte("test content")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write test msg: %v", err)
	}

	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "test_msg", "test_failure", "test")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	// Manually set retry_count to MaxRetries
	env, body, _ := ReadDLQEnvelopePath(dlqPath)
	env.RetryCount = MaxRetries
	data, _ := serializeDLQMessage(*env, body)
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("update DLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry should fail without --force
	err = RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, false)
	if err == nil {
		t.Errorf("expected error due to max retries")
	}
	if !strings.Contains(err.Error(), "max retries") {
		t.Errorf("expected 'max retries' error, got: %v", err)
	}

	// Retry with --force should succeed
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, true); err != nil {
		t.Fatalf("RetryFromDLQ with force: %v", err)
	}
}

func createDLQMessage(t *testing.T, root, agent, filename string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), content, 0o600); err != nil {
		t.Fatalf("write source message: %v", err)
	}
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), agent, filename, strings.TrimSuffix(filename, ".md"), "test_failure", "test detail")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	return dlqPath
}

func TestFindDLQMessage(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create DLQ message in new
	dlqNew := AgentDLQNew(root, "alice")
	filename := "dlq_test.md"
	if err := os.WriteFile(filepath.Join(dlqNew, filename), []byte("test"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Find in new
	path, box, err := FindDLQMessage(openDeliveryRootForTest(t, root), "alice", filename)
	if err != nil {
		t.Fatalf("FindDLQMessage: %v", err)
	}
	if box != BoxNew {
		t.Errorf("expected box 'new', got %s", box)
	}
	if !strings.HasSuffix(path, filename) {
		t.Errorf("path should end with filename")
	}

	// Move to cur
	if err := MoveDLQNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err != nil {
		t.Fatalf("MoveDLQNewToCur: %v", err)
	}

	// Find in cur
	_, box, err = FindDLQMessage(openDeliveryRootForTest(t, root), "alice", filename)
	if err != nil {
		t.Fatalf("FindDLQMessage after move: %v", err)
	}
	if box != BoxCur {
		t.Errorf("expected box 'cur', got %s", box)
	}
}
