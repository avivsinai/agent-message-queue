package fsq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invalidMessageFilenames(t *testing.T) []string {
	t.Helper()
	return []string{
		"../message.md",
		"nested/message.md",
		`nested\message.md`,
		filepath.Join(os.TempDir(), "message.md"),
		"message\x00.md",
		"message.txt",
	}
}

func TestValidateMessageFilename(t *testing.T) {
	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := ValidateMessageFilename(filename); err == nil {
				t.Fatalf("ValidateMessageFilename(%q) error = nil, want error", filename)
			}
		})
	}

	if err := ValidateMessageFilename("message.md"); err != nil {
		t.Fatalf("ValidateMessageFilename(valid) error = %v, want nil", err)
	}
}

func TestMessageFilenameEntryPointsAcceptValidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	inboxFilename := "valid_msg.md"
	inboxPath := filepath.Join(AgentInboxNew(root, "alice"), inboxFilename)
	if err := os.WriteFile(inboxPath, []byte("test content"), 0o600); err != nil {
		t.Fatalf("write inbox message: %v", err)
	}

	path, box, err := FindMessage(root, "alice", inboxFilename)
	if err != nil {
		t.Fatalf("FindMessage: %v", err)
	}
	if path != inboxPath || box != BoxNew {
		t.Fatalf("FindMessage = (%q, %q), want (%q, %q)", path, box, inboxPath, BoxNew)
	}
	if err := MoveNewToCur(openDeliveryRootForTest(t, root), "alice", inboxFilename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxCur(root, "alice"), inboxFilename)); err != nil {
		t.Fatalf("expected inbox cur file: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "valid_dlq_source.md", []byte("dlq content"))
	dlqFilename := filepath.Base(dlqPath)
	path, box, err = FindDLQMessage(openDeliveryRootForTest(t, root), "alice", dlqFilename)
	if err != nil {
		t.Fatalf("FindDLQMessage: %v", err)
	}
	wantDLQPath := filepath.Join("agents", "alice", "dlq", "new", dlqFilename)
	if path != wantDLQPath || box != BoxNew {
		t.Fatalf("FindDLQMessage = (%q, %q), want (%q, %q)", path, box, wantDLQPath, BoxNew)
	}
	if err := MoveDLQNewToCur(openDeliveryRootForTest(t, root), "alice", dlqFilename); err != nil {
		t.Fatalf("MoveDLQNewToCur: %v", err)
	}
	dlqCurPath := filepath.Join(AgentDLQCur(root, "alice"), dlqFilename)
	if _, err := os.Stat(dlqCurPath); err != nil {
		t.Fatalf("expected dlq cur file: %v", err)
	}
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, false); err != nil {
		t.Fatalf("RetryFromDLQ: %v", err)
	}
	restoredPath := filepath.Join(AgentInboxNew(root, "alice"), "valid_dlq_source.md")
	if _, err := os.Stat(restoredPath); err != nil {
		t.Fatalf("expected retry to restore original filename unchanged: %v", err)
	}
}

func TestFindMessageRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if _, _, err := FindMessage(root, "alice", filename); err == nil {
				t.Fatalf("FindMessage(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestMoveNewToCurRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := MoveNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err == nil {
				t.Fatalf("MoveNewToCur(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestMoveCurToDLQRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if _, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "msg", "test_failure", "test detail"); err == nil {
				t.Fatalf("MoveCurToDLQ(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestFindDLQMessageRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if _, _, err := FindDLQMessage(openDeliveryRootForTest(t, root), "alice", filename); err == nil {
				t.Fatalf("FindDLQMessage(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestMoveDLQNewToCurRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := MoveDLQNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err == nil {
				t.Fatalf("MoveDLQNewToCur(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestRetryFromDLQRejectsInvalidDLQFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filename, false); err == nil {
				t.Fatalf("RetryFromDLQ(%q) error = nil, want error", filename)
			}
		})
	}
}

func TestRetryFromDLQRejectsInvalidOriginalFilenames(t *testing.T) {
	for _, originalFile := range invalidMessageFilenames(t) {
		t.Run(originalFile, func(t *testing.T) {
			root := t.TempDir()
			if err := EnsureAgentDirs(root, "alice"); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			dlqPath := createDLQMessage(t, root, "alice", "safe_msg.md", []byte("test content"))
			env, body, err := ReadDLQEnvelopePath(dlqPath)
			if err != nil {
				t.Fatalf("ReadDLQEnvelope: %v", err)
			}
			env.OriginalFile = originalFile
			data, err := serializeDLQMessage(*env, body)
			if err != nil {
				t.Fatalf("serialize tampered envelope: %v", err)
			}
			if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
				t.Fatalf("write tampered envelope: %v", err)
			}

			err = RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false)
			if err == nil {
				t.Fatal("expected invalid original_file to be rejected")
			}
			if !strings.Contains(err.Error(), "invalid original_file") {
				t.Fatalf("expected invalid original_file error, got: %v", err)
			}
		})
	}
}
