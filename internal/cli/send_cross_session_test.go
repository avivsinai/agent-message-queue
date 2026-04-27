package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestSendFromSessionPreBootCrossSession(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "cto")
	targetRoot := filepath.Join(baseRoot, "qa")
	ensureSendSessionAgent(t, sourceRoot, "alice")
	ensureSendSessionAgent(t, targetRoot, "bob")

	output := runSendJSONForTest(t,
		"--me", "alice",
		"--root", baseRoot,
		"--from-session", "cto",
		"--to", "bob",
		"--session", "qa",
		"--body", "hello from setup terminal",
		"--json",
	)

	if got := output["root"]; got != targetRoot {
		t.Fatalf("root = %v, want %q", got, targetRoot)
	}
	if got := output["source_root"]; got != sourceRoot {
		t.Fatalf("source_root = %v, want %q", got, sourceRoot)
	}
	if output["cross_session"] != true {
		t.Fatal("expected cross_session=true")
	}
	if got := output["source_session"]; got != "cto" {
		t.Fatalf("source_session = %v, want cto", got)
	}
	if got := output["target_session"]; got != "qa" {
		t.Fatalf("target_session = %v, want qa", got)
	}

	targetEntries := mustReadDir(t, filepath.Join(targetRoot, "agents", "bob", "inbox", "new"))
	if len(targetEntries) != 1 {
		t.Fatalf("expected 1 target inbox message, got %d", len(targetEntries))
	}
	sourceEntries := mustReadDir(t, filepath.Join(sourceRoot, "agents", "alice", "outbox", "sent"))
	if len(sourceEntries) != 1 {
		t.Fatalf("expected 1 source outbox copy, got %d", len(sourceEntries))
	}
	baseEntries := mustReadDir(t, filepath.Join(baseRoot, "agents", "alice", "outbox", "sent"))
	if len(baseEntries) != 0 {
		t.Fatalf("expected no base-root outbox copy, got %d", len(baseEntries))
	}

	messagePath := filepath.Join(targetRoot, "agents", "bob", "inbox", "new", targetEntries[0].Name())
	header, err := format.ReadHeaderFile(messagePath)
	if err != nil {
		t.Fatalf("read target header: %v", err)
	}
	if header.ReplyTo != "alice@cto" {
		t.Fatalf("reply_to = %q, want alice@cto", header.ReplyTo)
	}
	if header.Thread != "p2p/cto:alice__qa:bob" {
		t.Fatalf("thread = %q, want p2p/cto:alice__qa:bob", header.Thread)
	}
}

func TestSendFromSessionRejectsProject(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	ensureSendSessionAgent(t, filepath.Join(baseRoot, "cto"), "alice")

	err := runSend([]string{
		"--me", "alice",
		"--root", baseRoot,
		"--from-session", "cto",
		"--to", "bob",
		"--session", "qa",
		"--project", "peer-project",
		"--body", "not allowed",
	})
	if err == nil {
		t.Fatal("expected --from-session with --project to fail")
	}
	if !strings.Contains(err.Error(), "--from-session is not supported with --project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendFromSessionRequiresExistingSourceSession(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	ensureSendSessionAgent(t, filepath.Join(baseRoot, "qa"), "bob")

	err := runSend([]string{
		"--me", "alice",
		"--root", baseRoot,
		"--from-session", "cto",
		"--to", "bob",
		"--session", "qa",
		"--body", "missing source",
	})
	if err == nil {
		t.Fatal("expected missing source session to fail")
	}
	if !strings.Contains(err.Error(), `source session "cto" not found`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

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

func runSendJSONForTest(t *testing.T, args ...string) map[string]any {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
	})

	err = runSend(args)

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
	return result
}

func ensureSendSessionAgent(t *testing.T, root, agent string) {
	t.Helper()

	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs(%q): %v", root, err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs(%q, %q): %v", root, agent, err)
	}
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	return entries
}
