package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

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

func TestDrainInboxItemsReturnsClaimedResultsWithLaterPostClaimError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverGuardMessage(t, root, "alice", "a-complete")
	deliverGuardMessage(t, root, "alice", "b-interrupted")
	injectedErr := errors.New("injected post-claim failure")

	items, err := drainInboxItemsWithClaimHook(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		func(claimed string) error {
			if claimed == "b-interrupted.md" {
				return injectedErr
			}
			return nil
		},
	)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("drain error = %v, want injected post-claim failure", err)
	}
	if len(items) != 2 {
		t.Fatalf("returned items = %d, want both committed claims: %#v", len(items), items)
	}
	itemsByID := make(map[string]inboxItem, len(items))
	for _, item := range items {
		itemsByID[item.ID] = item
	}
	if item := itemsByID["a-complete"]; item.ID != "a-complete" || !item.MovedToCur || item.ParseError != "" {
		t.Fatalf("completed first claim was hidden or changed: %#v", item)
	}
	if item := itemsByID["b-interrupted"]; item.ID != "b-interrupted" ||
		item.ParseError == "" ||
		item.FailureReason != "processing_error" ||
		item.MovedToCur ||
		item.MovedToDLQ {
		t.Fatalf("interrupted committed claim was not surfaced: %#v", item)
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

// A sandbox that grants write access only to <root>/agents/<me>/ relies on
// drain writing nothing else, as `amq drain --help` documents.
func TestRunDrainWritesOnlyItsDocumentedSet(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	msg := format.Message{Header: format.Header{Schema: 1, ID: "good", From: "bob", To: []string{"alice"}, Thread: "p2p/alice__bob", Subject: "Hi", Created: "2025-12-24T10:00:00Z"}, Body: "Hello"}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "good.md", data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "alice"), "corrupt.md"), []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	type stamp struct {
		mod  time.Time
		size int64
	}
	snapshot := func() map[string]stamp {
		out := map[string]stamp{}
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out[filepath.ToSlash(rel)] = stamp{info.ModTime(), info.Size()}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	allowed := func(rel string) bool {
		if rel == "agents/alice" {
			return true // the parent of presence.json and .presence.lock
		}
		for _, p := range []string{"inbox/new", "inbox/cur", "receipts", "receipts-pending", "dlq/tmp", "dlq/new", ".presence.lock", "presence.json"} {
			p = "agents/alice/" + p
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	// A write in the same clock tick as the setup must still change a time.
	time.Sleep(10 * time.Millisecond)
	before := snapshot()

	if result := runDrainJSON(t, root, "alice", 0, false); result.Count != 2 {
		t.Fatalf("drained %d messages, want 2", result.Count)
	}
	after := snapshot()
	for rel, s := range after {
		if old, ok := before[rel]; (!ok || old != s) && !allowed(rel) {
			t.Errorf("drain wrote %s outside its documented set", rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok && !allowed(rel) {
			t.Errorf("drain removed %s outside its documented set", rel)
		}
	}
}
