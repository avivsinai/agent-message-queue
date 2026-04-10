package receipt

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func setupTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, agent := range []string{"claude", "codex"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestEmitAndRead(t *testing.T) {
	root := setupTestRoot(t)

	r := New("msg_001", "p2p/claude__codex", "claude", "codex", StageDrained, "")
	if err := Emit(root, r); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Consumer-local receipt should exist.
	consumerPath := filepath.Join(root, "agents", "codex", "receipts", r.filename())
	if _, err := os.Stat(consumerPath); err != nil {
		t.Fatalf("consumer receipt missing: %v", err)
	}

	// Read back and verify.
	got, err := Read(consumerPath)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.MsgID != "msg_001" || got.Stage != StageDrained || got.Sender != "claude" || got.Consumer != "codex" {
		t.Errorf("unexpected receipt: %+v", got)
	}
}

func TestEmitSelfSend(t *testing.T) {
	root := setupTestRoot(t)

	r := New("msg_002", "", "claude", "claude", StageDLQ, "")
	if err := Emit(root, r); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	receipts, err := List(root, "claude", ListFilter{MsgID: "msg_002"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(receipts))
	}
}

func TestListFilter(t *testing.T) {
	root := setupTestRoot(t)

	// Emit multiple receipts.
	for _, tc := range []struct {
		msgID, stage string
	}{
		{"msg_010", StageDrained},
		{"msg_010", StageDLQ},
		{"msg_011", StageDrained},
		{"msg_012", StageDLQ},
	} {
		r := New(tc.msgID, "p2p/claude__codex", "claude", "codex", tc.stage, "")
		if err := Emit(root, r); err != nil {
			t.Fatalf("Emit %s/%s: %v", tc.msgID, tc.stage, err)
		}
	}

	// Filter by msg_id.
	got, err := List(root, "codex", ListFilter{MsgID: "msg_010"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("msg_010 filter: expected 2, got %d", len(got))
	}

	// Filter by stage.
	got, err = List(root, "codex", ListFilter{Stage: StageDrained})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("drained filter: expected 2, got %d", len(got))
	}

	// Filter by stage=dlq.
	got, err = List(root, "codex", ListFilter{Stage: StageDLQ})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("dlq filter: expected 2, got %d", len(got))
	}

	// No filter.
	got, err = List(root, "codex", ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Errorf("no filter: expected 4, got %d", len(got))
	}
}

func TestListEmptyDir(t *testing.T) {
	root := setupTestRoot(t)

	// No receipts yet — should return nil, nil.
	got, err := List(root, "codex", ListFilter{})
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestDLQReceiptDetail(t *testing.T) {
	root := setupTestRoot(t)

	r := New("msg_030", "", "claude", "codex", StageDLQ, "parse_error: invalid JSON header")
	if err := Emit(root, r); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got, err := Read(filepath.Join(root, "agents", "codex", "receipts", r.filename()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Detail != "parse_error: invalid JSON header" {
		t.Errorf("expected detail, got %q", got.Detail)
	}
}

func TestWaitForCrossRoot(t *testing.T) {
	sourceRoot := t.TempDir()
	deliveryRoot := setupTestRoot(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(100 * time.Millisecond)
		r := New("msg_cross_root", "p2p/claude__codex", "claude", "codex", StageDrained, "")
		_ = Emit(deliveryRoot, r)
	}()

	got, err := WaitFor(deliveryRoot, "msg_cross_root", "codex", StageDrained, 2*time.Second, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitFor(deliveryRoot): %v", err)
	}
	if got.Consumer != "codex" || got.MsgID != "msg_cross_root" {
		t.Fatalf("unexpected receipt: %+v", got)
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("emit goroutine did not finish")
	}

	if _, err := WaitFor(sourceRoot, "msg_cross_root", "codex", StageDrained, 150*time.Millisecond, 25*time.Millisecond); !os.IsTimeout(err) && err != os.ErrDeadlineExceeded {
		t.Fatalf("WaitFor(sourceRoot) error = %v, want deadline exceeded", err)
	}
}
