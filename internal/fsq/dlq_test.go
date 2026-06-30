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

func TestMoveToDLQClaimsBeforeDLQDelivery(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	filename := "claim_before_dlq.md"
	content := []byte("not valid frontmatter")
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	dlqTmp := AgentDLQTmp(root, "alice")
	if err := os.RemoveAll(dlqTmp); err != nil {
		t.Fatalf("remove dlq tmp: %v", err)
	}
	if err := os.WriteFile(dlqTmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block dlq tmp: %v", err)
	}

	_, err := MoveToDLQ(root, "alice", filename, "claim_before_dlq", "parse_error", "missing frontmatter")
	if err == nil {
		t.Fatal("expected MoveToDLQ to fail when DLQ delivery cannot create tmp dir")
	}

	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("source should be claimed out of inbox/new before DLQ delivery, stat err: %v", err)
	}
	claimedContent, err := os.ReadFile(filepath.Join(AgentInboxCur(root, "alice"), filename))
	if err != nil {
		t.Fatalf("claimed source should remain in inbox/cur: %v", err)
	}
	if string(claimedContent) != string(content) {
		t.Fatalf("claimed content mismatch: got %q", claimedContent)
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
	if err := MoveNewToCur(root, "alice", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}

	dlqPath, err := MoveCurToDLQ(root, "alice", filename, "claimed_corrupt", "parse_error", "missing frontmatter")
	if err != nil {
		t.Fatalf("MoveCurToDLQ: %v", err)
	}

	if _, err := os.Stat(filepath.Join(AgentInboxCur(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("claimed original should be removed from inbox/cur")
	}

	env, body, err := ReadDLQEnvelope(dlqPath)
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

func TestReadDLQEnvelopeRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dlqPath := createDLQMessage(t, root, "alice", "symlink_source.md", []byte("test content"))
	link := filepath.Join(AgentDLQNew(root, "alice"), "symlink_dlq.md")
	if err := os.Symlink(dlqPath, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := ReadDLQEnvelope(link)
	if err == nil {
		t.Fatal("expected symlink DLQ envelope to be rejected")
	}
}

func TestMoveCurToDLQRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("target content"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(AgentInboxCur(root, "alice"), "symlink_source.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := MoveCurToDLQ(root, "alice", "symlink_source.md", "symlink_source", "parse_error", "test")
	if err == nil {
		t.Fatal("expected symlink inbox source to be rejected")
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("target should remain untouched: %v", statErr)
	}
}

func TestRetryFromDLQRejectsTraversalOriginalFile(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "safe_msg.md", []byte("test content"))
	env, body, err := ReadDLQEnvelope(dlqPath)
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

	err = RetryFromDLQ(root, "alice", filepath.Base(dlqPath), false)
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

func TestRetryFromDLQEnvelopeUpdateFailureReturnsErrorBeforeRedelivery(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "update_failure.md", []byte("test content"))
	dlqCur := AgentDLQCur(root, "alice")
	if err := os.RemoveAll(dlqCur); err != nil {
		t.Fatalf("remove dlq cur: %v", err)
	}
	if err := os.WriteFile(dlqCur, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block dlq cur: %v", err)
	}

	err := RetryFromDLQ(root, "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected RetryFromDLQ to return envelope update error")
	}
	if !strings.Contains(err.Error(), "dlq envelope") {
		t.Fatalf("expected dlq envelope error, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "alice"), "update_failure.md")); !os.IsNotExist(err) {
		t.Fatalf("retry should not redeliver before envelope update succeeds, stat err: %v", err)
	}
}

func TestRetryFromDLQRedeliveryFailureReturnsError(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "redelivery_failure.md", []byte("test content"))
	inboxTmp := AgentInboxTmp(root, "alice")
	if err := os.RemoveAll(inboxTmp); err != nil {
		t.Fatalf("remove inbox tmp: %v", err)
	}
	if err := os.WriteFile(inboxTmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block inbox tmp: %v", err)
	}

	err := RetryFromDLQ(root, "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected RetryFromDLQ to return redelivery error")
	}
	if !strings.Contains(err.Error(), "redeliver to inbox") {
		t.Fatalf("expected redelivery error, got: %v", err)
	}

	curPath := filepath.Join(AgentDLQCur(root, "alice"), filepath.Base(dlqPath))
	env, _, err := ReadDLQEnvelope(curPath)
	if err != nil {
		t.Fatalf("expected updated DLQ envelope in cur: %v", err)
	}
	if env.RetryCount != 1 {
		t.Fatalf("expected retry_count 1 after state transition, got %d", env.RetryCount)
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

func createDLQMessage(t *testing.T, root, agent, filename string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), content, 0o600); err != nil {
		t.Fatalf("write source message: %v", err)
	}
	dlqPath, err := MoveToDLQ(root, agent, filename, strings.TrimSuffix(filename, ".md"), "test_failure", "test detail")
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
