package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

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
	if _, err := deliverToInboxForTest(t, root, "alice", "test-msg-1.md", data); err != nil {
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

func TestRunDrainStrictAllowsReservedUserInbox(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	for _, agent := range []string{"claude", "codex", "user"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	writeKnownAgentsConfig(t, root, []string{"claude", "codex"})

	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      "operator-gate",
			From:    "claude",
			To:      []string{"user"},
			Thread:  "p2p/claude__user",
			Subject: "Need operator",
			Created: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: "Please decide.",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "user", "operator-gate.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	result := runDrainJSONStrict(t, root, "user")
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	item := result.Drained[0]
	if item.ID != "operator-gate" || item.ParseError != "" || !item.MovedToCur || item.MovedToDLQ {
		t.Fatalf("unexpected drain item: %+v", item)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "user"), "operator-gate.md")); err != nil {
		t.Fatalf("message should move to user cur: %v", err)
	}
	dlqEntries, err := os.ReadDir(fsq.AgentDLQNew(root, "user"))
	if err != nil {
		t.Fatalf("read user dlq: %v", err)
	}
	if len(dlqEntries) != 0 {
		t.Fatalf("expected no user DLQ entries, got %d", len(dlqEntries))
	}

	receipts, err := receipt.List(root, "user", receipt.ListFilter{
		MsgID: "operator-gate",
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 drained receipt, got %d", len(receipts))
	}
	if receipts[0].Sender != "claude" || receipts[0].Consumer != "user" {
		t.Fatalf("unexpected drained receipt: %+v", receipts[0])
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
	if _, err := deliverToInboxForTest(t, root, "alice", "body-test.md", data); err != nil {
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
		if _, err := deliverToInboxForTest(t, root, "alice", "body-test.md", data); err != nil {
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
		if _, err := deliverToInboxForTest(t, root, "alice", "body-test.md", data); err != nil {
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
		if _, err := deliverToInboxForTest(t, root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
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

func TestDrainInboxItemsConcurrentClaimsSingleWinner(t *testing.T) {
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
			Schema:  format.CurrentSchema,
			ID:      "claim-once",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Claim once",
			Created: "2025-12-24T10:00:00Z",
		},
		Body: "Only one drain may consume this.",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "claim-once.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	const workers = 2
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	start := make(chan struct{})
	results := make(chan []inboxItem, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			items, err := drainInboxItems(deliveryRoot, root, "alice", false, 0, &headerValidator{})
			if err != nil {
				errs <- err
				return
			}
			results <- items
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("drainInboxItems: %v", err)
	}

	total := 0
	for items := range results {
		total += len(items)
		for _, item := range items {
			if item.ID != "claim-once" {
				t.Fatalf("unexpected drained item: %+v", item)
			}
			if !item.MovedToCur || item.MovedToDLQ {
				t.Fatalf("expected a single cur claim, got %+v", item)
			}
		}
	}
	if total != 1 {
		t.Fatalf("expected exactly one drained item across concurrent drains, got %d", total)
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "claim-once",
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected exactly one drained receipt, got %d", len(receipts))
	}
}

func TestClaimMailboxDirsExistRejectsMissingCur(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := os.RemoveAll(fsq.AgentInboxCur(root, "alice")); err != nil {
		t.Fatalf("remove inbox/cur: %v", err)
	}

	exists, err := claimMailboxDirsExist(openDeliveryRootForCLITest(t, root), "alice")
	if err != nil {
		t.Fatalf("claimMailboxDirsExist: %v", err)
	}
	if exists {
		t.Fatal("missing inbox/cur must not be classified as a concurrent message claim")
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
		if _, err := deliverToInboxForTest(t, root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
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

func runDrainJSONStrict(t *testing.T, root, agent string) drainResult {
	t.Helper()
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain([]string{"--root", root, "--me", agent, "--strict", "--json"})

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
