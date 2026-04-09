package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestSendCrossSessionWithExplicitRootOverride(t *testing.T) {
	staleBase := filepath.Join(t.TempDir(), "stale-base")
	t.Setenv("AM_ROOT", filepath.Join(staleBase, "session1"))
	t.Setenv("AM_BASE_ROOT", staleBase)

	projectDir := t.TempDir()
	baseRoot := filepath.Join(projectDir, ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "session2")
	targetRoot := filepath.Join(baseRoot, "session3")

	if err := os.MkdirAll(filepath.Join(sourceRoot, "agents", "claude", "outbox", "sent"), 0o700); err != nil {
		t.Fatalf("mkdir source outbox: %v", err)
	}
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(targetRoot, "agents", "codex", "inbox", sub), 0o700); err != nil {
			t.Fatalf("mkdir target inbox: %v", err)
		}
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
	})

	err = runSend([]string{
		"--me", "claude",
		"--root", sourceRoot,
		"--to", "codex",
		"--session", "session3",
		"--body", "hello across sessions",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	var result map[string]any
	if err := json.Unmarshal(buf[:n], &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, buf[:n])
	}

	if got := result["root"]; got != targetRoot {
		t.Fatalf("root = %v, want %q", got, targetRoot)
	}
	if result["cross_session"] != true {
		t.Fatal("expected cross_session=true")
	}
	if got := result["source_session"]; got != "session2" {
		t.Fatalf("source_session = %v, want %q", got, "session2")
	}
	if got := result["target_session"]; got != "session3" {
		t.Fatalf("target_session = %v, want %q", got, "session3")
	}

	inbox := filepath.Join(targetRoot, "agents", "codex", "inbox", "new")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in inbox, got %d", len(entries))
	}
}

func TestSendCrossSessionWaitForUsesDeliveryRoot(t *testing.T) {
	staleBase := filepath.Join(t.TempDir(), "stale-base")
	t.Setenv("AM_ROOT", filepath.Join(staleBase, "session1"))
	t.Setenv("AM_BASE_ROOT", staleBase)

	projectDir := t.TempDir()
	baseRoot := filepath.Join(projectDir, ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "session2")
	targetRoot := filepath.Join(baseRoot, "session3")

	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, "agents", "claude", "outbox", "sent"), 0o700); err != nil {
			t.Fatalf("mkdir source outbox: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(sourceRoot, "agents", "claude", "inbox", sub), 0o700); err != nil {
			t.Fatalf("mkdir source inbox: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(targetRoot, "agents", "codex", "inbox", sub), 0o700); err != nil {
			t.Fatalf("mkdir target inbox: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(targetRoot, "agents", "codex", "receipts"), 0o700); err != nil {
		t.Fatalf("mkdir target receipts: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			entries, err := os.ReadDir(filepath.Join(targetRoot, "agents", "codex", "inbox", "new"))
			if err == nil && len(entries) == 1 {
				msgID := entries[0].Name()[:len(entries[0].Name())-3]
				r := receipt.New(msgID, "", "claude", "codex", receipt.StageDrained, "")
				_ = receipt.Emit(targetRoot, r)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
	})

	err = runSend([]string{
		"--me", "claude",
		"--root", sourceRoot,
		"--to", "codex",
		"--session", "session3",
		"--body", "hello across sessions",
		"--wait-for", "drained",
		"--wait-timeout", "2s",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	var result map[string]any
	if err := json.Unmarshal(buf[:n], &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, buf[:n])
	}

	wait, ok := result["wait"].(map[string]any)
	if !ok {
		t.Fatalf("expected wait object in result, got %v", result["wait"])
	}
	if got := wait["event"]; got != "matched" {
		t.Fatalf("wait.event = %v, want matched", got)
	}
	receiptObj, ok := wait["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested receipt object, got %v", wait["receipt"])
	}
	if got := receiptObj["consumer"]; got != "codex" {
		t.Fatalf("receipt.consumer = %v, want codex", got)
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("receipt emitter goroutine did not finish")
	}
}
