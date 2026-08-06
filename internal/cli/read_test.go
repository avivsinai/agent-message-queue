package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunReadOutputsCommittedClaimBeforeDurabilityError(t *testing.T) {
	for _, test := range []struct {
		name        string
		jsonOutput  bool
		injectedErr error
	}{
		{name: "text", injectedErr: errors.New("injected read claim sync failure")},
		{name: "json wrapping not-exist", jsonOutput: true, injectedErr: os.ErrNotExist},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice", "bob")
			const id = "read-committed"
			deliverCommittedClaimFixture(t, root, "alice", id, "2026-07-28T03:00:00Z")
			installReadCommittedClaimAfterMove(t, id+".md", test.injectedErr)

			args := []string{"--root", root, "--me", "alice", "--id", id}
			if test.jsonOutput {
				args = append(args, "--json")
			}
			stdout, _, err := captureEnvOutput(t, func() error {
				return runRead(args)
			})
			var committed *fsq.CommittedDurabilityError
			if !errors.As(err, &committed) || !errors.Is(err, test.injectedErr) {
				t.Fatalf("read error = %T %v, want committed injected error", err, err)
			}

			if test.jsonOutput {
				var result struct {
					Header format.Header `json:"header"`
					Body   string        `json:"body"`
				}
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatalf("decode committed read output: %v (output: %s)", err, stdout)
				}
				if result.Header.ID != id || result.Body != "body for "+id+"\n" {
					t.Fatalf("committed read JSON = %#v", result)
				}
			} else if stdout != "body for "+id+"\n" {
				t.Fatalf("committed read text = %q", stdout)
			}

			assertReadClaimStateAndReceipt(t, root, id, 1)

			retryOut, _, retryErr := captureEnvOutput(t, func() error {
				return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
			})
			if retryErr != nil {
				t.Fatalf("retry committed read: %v", retryErr)
			}
			var retry struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal([]byte(retryOut), &retry); err != nil || retry.Body != "body for "+id+"\n" {
				t.Fatalf("retry output = %q, decode err=%v", retryOut, err)
			}
			assertReadClaimStateAndReceipt(t, root, id, 1)
		})
	}
}

func installReadCommittedClaimAfterMove(t *testing.T, targetFilename string, injectedErr error) {
	t.Helper()
	oldClaim := claimInboxNewToCur
	claimInboxNewToCur = func(root *fsq.DeliveryRoot, agent, filename string) error {
		if err := fsq.MoveNewToCur(root, agent, filename); err != nil {
			return err
		}
		if filename != targetFilename {
			return nil
		}
		return &fsq.CommittedDurabilityError{
			FinalPath: root.DisplayPath(filepath.Join("agents", agent, "inbox", "cur", filename)),
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { claimInboxNewToCur = oldClaim })
}

func assertReadClaimStateAndReceipt(t *testing.T, root, id string, wantReceipts int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md")); !os.IsNotExist(err) {
		t.Fatalf("claimed message remains in new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")); err != nil {
		t.Fatalf("claimed message is not in cur: %v", err)
	}
	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: id,
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("list drained receipts: %v", err)
	}
	if len(receipts) != wantReceipts {
		t.Fatalf("drained receipts = %d, want %d", len(receipts), wantReceipts)
	}
}

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
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-read-001.md", data); err != nil {
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
