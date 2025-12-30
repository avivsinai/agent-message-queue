package fsq

import (
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
	dlqPath, err := MoveToDLQ(root, "alice", filename, "corrupt_123", "parse_error", "missing frontmatter")
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
	env, body, err := ReadDLQEnvelope(dlqPath)
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
	dlqPath, err := MoveToDLQ(root, "alice", filename, "test_msg", "test_failure", "test detail")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry from DLQ
	if err := RetryFromDLQ(root, "alice", dlqFilename, false); err != nil {
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
	env, _, err := ReadDLQEnvelope(curPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope from cur: %v", err)
	}
	if env.RetryCount != 1 {
		t.Errorf("expected retry_count 1 after retry, got %d", env.RetryCount)
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

	dlqPath, err := MoveToDLQ(root, "alice", filename, "test_msg", "test_failure", "test")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	// Manually set retry_count to MaxRetries
	env, body, _ := ReadDLQEnvelope(dlqPath)
	env.RetryCount = MaxRetries
	data, _ := serializeDLQMessage(*env, body)
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("update DLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry should fail without --force
	err = RetryFromDLQ(root, "alice", dlqFilename, false)
	if err == nil {
		t.Errorf("expected error due to max retries")
	}
	if !strings.Contains(err.Error(), "max retries") {
		t.Errorf("expected 'max retries' error, got: %v", err)
	}

	// Retry with --force should succeed
	if err := RetryFromDLQ(root, "alice", dlqFilename, true); err != nil {
		t.Fatalf("RetryFromDLQ with force: %v", err)
	}
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
	path, box, err := FindDLQMessage(root, "alice", filename)
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
	if err := MoveDLQNewToCur(root, "alice", filename); err != nil {
		t.Fatalf("MoveDLQNewToCur: %v", err)
	}

	// Find in cur
	_, box, err = FindDLQMessage(root, "alice", filename)
	if err != nil {
		t.Fatalf("FindDLQMessage after move: %v", err)
	}
	if box != BoxCur {
		t.Errorf("expected box 'cur', got %s", box)
	}
}
