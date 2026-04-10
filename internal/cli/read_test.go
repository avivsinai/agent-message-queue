package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunReadInvalidHeaderMovesToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      "msg-read-001",
			From:    "Bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Created: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: "hello\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-read-001.md", data); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}

	err = runRead([]string{"--me", "alice", "--root", root, "--id", "msg-read-001"})
	if err == nil {
		t.Fatal("expected runRead to fail on invalid header")
	}
	if !strings.Contains(err.Error(), "invalid message header msg-read-001") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg-read-001.md")); !os.IsNotExist(err) {
		t.Fatalf("expected message removed from inbox/new, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), "msg-read-001.md")); !os.IsNotExist(err) {
		t.Fatalf("expected message absent from inbox/cur, stat err = %v", err)
	}

	dlqEntries, err := os.ReadDir(fsq.AgentDLQNew(root, "alice"))
	if err != nil {
		t.Fatalf("ReadDir(dlq/new): %v", err)
	}
	if len(dlqEntries) != 1 {
		t.Fatalf("expected 1 message in dlq/new, got %d", len(dlqEntries))
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "msg-read-001",
		Stage: receipt.StageDLQ,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 dlq receipt, got %d", len(receipts))
	}
	if receipts[0].Detail == "" {
		t.Fatal("expected dlq receipt detail")
	}
}

func TestRunReadParseErrorMovesToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg-read-corrupt.md"), []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := runRead([]string{"--me", "alice", "--root", root, "--id", "msg-read-corrupt"})
	if err == nil {
		t.Fatal("expected runRead to fail on parse error")
	}
	if !strings.Contains(err.Error(), "failed to parse message msg-read-corrupt") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg-read-corrupt.md")); !os.IsNotExist(err) {
		t.Fatalf("expected message removed from inbox/new, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), "msg-read-corrupt.md")); !os.IsNotExist(err) {
		t.Fatalf("expected message absent from inbox/cur, stat err = %v", err)
	}

	dlqEntries, err := os.ReadDir(fsq.AgentDLQNew(root, "alice"))
	if err != nil {
		t.Fatalf("ReadDir(dlq/new): %v", err)
	}
	if len(dlqEntries) != 1 {
		t.Fatalf("expected 1 message in dlq/new, got %d", len(dlqEntries))
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "msg-read-corrupt",
		Stage: receipt.StageDLQ,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 dlq receipt, got %d", len(receipts))
	}
	if receipts[0].Sender != "" {
		t.Fatalf("expected empty sender for parse_error receipt, got %q", receipts[0].Sender)
	}
	if receipts[0].Detail == "" {
		t.Fatal("expected dlq receipt detail")
	}
}
