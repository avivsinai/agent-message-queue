package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunDrainEmpty(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	t.Run("empty inbox returns empty JSON", func(t *testing.T) {
		result := runDrainJSON(t, root, "alice", 0, false)
		if result.Count != 0 {
			t.Errorf("expected count 0, got %d", result.Count)
		}
		if len(result.Drained) != 0 {
			t.Errorf("expected empty drained, got %d items", len(result.Drained))
		}
	})

	t.Run("empty inbox silent in text mode", func(t *testing.T) {
		output := runDrainText(t, root, "alice", 0, false)
		if output != "" {
			t.Errorf("expected empty output, got %q", output)
		}
	})
}

func TestRunDrainMovesToCur(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "test-msg-1",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Test message",
			Created: "2025-12-24T10:00:00Z",
		},
		Body: "Hello Alice!",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "test-msg-1.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Verify message is in new
	newPath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("message should be in new: %v", err)
	}

	// Drain
	result := runDrainJSON(t, root, "alice", 0, false)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ID != "test-msg-1" {
		t.Errorf("expected ID test-msg-1, got %s", result.Drained[0].ID)
	}
	if !result.Drained[0].MovedToCur {
		t.Errorf("expected MovedToCur=true")
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(curPath); err != nil {
		t.Errorf("message should be in cur: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("message should NOT be in new anymore")
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "test-msg-1",
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 drained receipt, got %d", len(receipts))
	}
	if receipts[0].Sender != "bob" || receipts[0].Consumer != "alice" {
		t.Errorf("unexpected drained receipt: %+v", receipts[0])
	}

	// Second drain should return empty
	result2 := runDrainJSON(t, root, "alice", 0, false)
	if result2.Count != 0 {
		t.Errorf("second drain should be empty, got %d", result2.Count)
	}
}

func TestRunDrainWithBody(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "body-test",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "With body",
			Created: "2025-12-24T11:00:00Z",
		},
		Body: "This is the message body.",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	t.Run("without include-body", func(t *testing.T) {
		// Need to re-create since previous test moved it
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, false)
		if result.Drained[0].Body != "" {
			t.Errorf("expected empty body, got %q", result.Drained[0].Body)
		}
	})

	t.Run("with include-body", func(t *testing.T) {
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, true)
		if result.Drained[0].Body != "This is the message body.\n" {
			t.Errorf("expected body, got %q", result.Drained[0].Body)
		}
	})
}

func TestRunDrainLimit(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	// Drain with limit 2
	result := runDrainJSON(t, root, "alice", 2, false)
	if result.Count != 2 {
		t.Errorf("expected count 2, got %d", result.Count)
	}

	// Verify only 2 moved to cur
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(root, "alice"))
	if len(curEntries) != 2 {
		t.Errorf("expected 2 in cur, got %d", len(curEntries))
	}

	// Verify 3 still in new
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(root, "alice"))
	if len(newEntries) != 3 {
		t.Errorf("expected 3 in new, got %d", len(newEntries))
	}
}

func TestRunDrainCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Write a corrupt message directly
	newDir := fsq.AgentInboxNew(root, "alice")
	corruptPath := filepath.Join(newDir, "corrupt.md")
	if err := os.WriteFile(corruptPath, []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ParseError == "" {
		t.Errorf("expected parse error for corrupt message")
	}
	if !result.Drained[0].MovedToDLQ {
		t.Errorf("corrupt message should be moved to DLQ")
	}

	// Verify corrupt message moved to DLQ (not cur)
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "corrupt.md")
	if _, err := os.Stat(curPath); err == nil {
		t.Errorf("corrupt message should NOT be in cur (should be in DLQ)")
	}

	// Verify message is in DLQ
	dlqNewDir := fsq.AgentDLQNew(root, "alice")
	entries, err := os.ReadDir(dlqNewDir)
	if err != nil {
		t.Fatalf("read DLQ dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 DLQ message, got %d", len(entries))
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "corrupt",
		Stage: receipt.StageDLQ,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 dlq receipt, got %d", len(receipts))
	}
	if receipts[0].Detail == "" {
		t.Errorf("expected dlq receipt detail to be populated")
	}
}

func TestRunDrainSorting(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages out of order (filesystem order != timestamp order)
	timestamps := []string{
		"2025-12-24T10:00:03Z",
		"2025-12-24T10:00:01Z",
		"2025-12-24T10:00:02Z",
	}
	for i, ts := range timestamps {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Created: ts,
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	result := runDrainJSON(t, root, "alice", 0, false)
	if result.Count != 3 {
		t.Fatalf("expected 3, got %d", result.Count)
	}

	// Should be sorted by timestamp: b (01), c (02), a (03)
	expected := []string{"msg-b", "msg-c", "msg-a"}
	for i, exp := range expected {
		if result.Drained[i].ID != exp {
			t.Errorf("position %d: expected %s, got %s", i, exp, result.Drained[i].ID)
		}
	}
}

func runDrainJSON(t *testing.T, root, agent string, limit int, includeBody bool) drainResult {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json"}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result drainResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return result
}

func runDrainText(t *testing.T, root, agent string, limit int, includeBody bool) string {
	t.Helper()
	args := []string{"--root", root, "--me", agent}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}
