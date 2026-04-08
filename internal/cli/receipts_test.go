package cli

import (
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func setupReceiptsTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func deliverTestMsg(t *testing.T, root, from, to, msgID string) {
	t.Helper()
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      msgID,
			From:    from,
			To:      []string{to},
			Thread:  "p2p/" + from + "__" + to,
			Subject: "test",
			Created: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: "test body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsq.DeliverToInbox(root, to, msgID+".md", data); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptsListEmpty(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsList([]string{"--me", "alice", "--root", root, "--json"})
	})

	var result receiptsListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s", err, stdout)
	}
	if result.Count != 0 {
		t.Errorf("expected 0, got %d", result.Count)
	}
}

func TestReceiptsListAfterDrain(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	deliverTestMsg(t, root, "bob", "alice", "msg-r-001")

	// Drain to trigger drained receipt.
	captureOutput(t, func() error {
		return runDrain([]string{"--me", "alice", "--root", root, "--json"})
	})

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsList([]string{"--me", "alice", "--root", root, "--msg-id", "msg-r-001", "--json"})
	})

	var result receiptsListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s", err, stdout)
	}
	if result.Count != 1 {
		t.Fatalf("expected 1 receipt, got %d", result.Count)
	}
	if result.Receipts[0].Stage != receipt.StageDrained {
		t.Errorf("expected stage=drained, got %s", result.Receipts[0].Stage)
	}
	if result.Receipts[0].Sender != "bob" {
		t.Errorf("expected sender=bob, got %s", result.Receipts[0].Sender)
	}
}

func TestReceiptsListFilterByStage(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	// Emit receipts directly for testing filters.
	for _, stage := range []string{receipt.StageDrained, receipt.StageDLQ} {
		r := receipt.New("msg-f-001", "p2p/alice__bob", "bob", "alice", stage, "")
		if err := receipt.Emit(root, "alice", r); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsList([]string{"--me", "alice", "--root", root, "--stage", "dlq", "--json"})
	})

	var result receiptsListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Count != 1 {
		t.Errorf("expected 1 dlq receipt, got %d", result.Count)
	}
}

func TestReceiptsWaitImmediate(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	// Pre-emit a receipt so wait finds it immediately.
	r := receipt.New("msg-w-001", "", "bob", "alice", receipt.StageDrained, "")
	if err := receipt.Emit(root, "alice", r); err != nil {
		t.Fatal(err)
	}

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsWait([]string{
			"--me", "alice", "--root", root,
			"--msg-id", "msg-w-001", "--stage", "drained",
			"--timeout", "5s", "--json",
		})
	})

	var result receiptsWaitResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s", err, stdout)
	}
	if result.Event != "matched" {
		t.Errorf("expected matched, got %s", result.Event)
	}
	if result.Receipt == nil || result.Receipt.MsgID != "msg-w-001" {
		t.Errorf("unexpected receipt: %+v", result.Receipt)
	}
}

func TestReceiptsWaitTimeout(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsWait([]string{
			"--me", "alice", "--root", root,
			"--msg-id", "msg-nope", "--stage", "drained",
			"--timeout", "1s", "--poll-interval", "200ms", "--json",
		})
	})

	var result receiptsWaitResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s", err, stdout)
	}
	if result.Event != "timeout" {
		t.Errorf("expected timeout, got %s", result.Event)
	}
}

func TestReceiptsWaitDelayed(t *testing.T) {
	root := setupReceiptsTestRoot(t)

	// Emit receipt after a short delay.
	go func() {
		time.Sleep(500 * time.Millisecond)
		r := receipt.New("msg-d-001", "", "bob", "alice", receipt.StageDrained, "")
		_ = receipt.Emit(root, "alice", r)
	}()

	stdout, _ := captureOutput(t, func() error {
		return runReceiptsWait([]string{
			"--me", "alice", "--root", root,
			"--msg-id", "msg-d-001", "--stage", "drained",
			"--timeout", "5s", "--poll-interval", "200ms", "--json",
		})
	})

	var result receiptsWaitResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s", err, stdout)
	}
	if result.Event != "matched" {
		t.Errorf("expected matched, got %s", result.Event)
	}
}

// captureOutput redirects stdout/stderr for a function call and returns both.
func captureOutput(t *testing.T, fn func() error) (string, string) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	err := fn()

	wOut.Close()
	wErr.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	outBytes, _ := io.ReadAll(rOut)
	rOut.Close()

	errBytes, _ := io.ReadAll(rErr)
	rErr.Close()

	if err != nil {
		t.Logf("function returned error: %v", err)
	}

	return string(outBytes), string(errBytes)
}
