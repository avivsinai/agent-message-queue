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

func TestAbsoluteSessionRootResolvesRelativeAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := absoluteSessionRoot(filepath.Join(".agent-mail", "collab"))
	if err != nil {
		t.Fatalf("absoluteSessionRoot: %v", err)
	}
	want := filepath.Join(dir, ".agent-mail", "collab")
	if got != want {
		t.Fatalf("absoluteSessionRoot = %q, want %q", got, want)
	}
}

func TestAbsoluteSessionRootKeepsAbsoluteUnchanged(t *testing.T) {
	abs := filepath.Join(t.TempDir(), ".agent-mail", "collab")
	got, err := absoluteSessionRoot(abs)
	if err != nil {
		t.Fatalf("absoluteSessionRoot: %v", err)
	}
	if got != abs {
		t.Fatalf("absoluteSessionRoot = %q, want %q unchanged", got, abs)
	}
}

func TestEnvShellOutputEmitsAbsoluteRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	relRoot := filepath.Join(".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(filepath.Join(dir, relRoot)); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	t.Setenv("AM_ROOT", relRoot)
	t.Setenv("AM_BASE_ROOT", "")

	output, err := captureEnvStdout(t, func() error {
		return runEnv([]string{"--me", "claude"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	wantRoot := filepath.Join(dir, relRoot)
	if !strings.Contains(output, "export AM_ROOT="+wantRoot) &&
		!strings.Contains(output, "export AM_ROOT='"+wantRoot+"'") {
		t.Fatalf("shell output should pin absolute AM_ROOT %q, got:\n%s", wantRoot, output)
	}
}

func TestEnvExportEmitsAbsoluteRoots(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	relRoot := filepath.Join(".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(filepath.Join(dir, relRoot)); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	t.Setenv("AM_ROOT", relRoot)
	t.Setenv("AM_BASE_ROOT", ".agent-mail")

	output, err := captureEnvStdout(t, func() error {
		return runEnv([]string{"--me", "claude", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv --export: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		for _, prefix := range []string{"export AM_ROOT=", "export AM_BASE_ROOT="} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			value := strings.Trim(strings.TrimPrefix(line, prefix), "'")
			if !filepath.IsAbs(value) {
				t.Fatalf("%s should be absolute, got %q (output:\n%s)", prefix, value, output)
			}
		}
	}
	if !strings.Contains(output, "export AM_ROOT=") {
		t.Fatalf("expected AM_ROOT export, got:\n%s", output)
	}
}

func TestReplyWaitForDrained(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       originalID,
			From:     "bob",
			To:       []string{"alice"},
			Thread:   "p2p/alice__bob",
			Subject:  "Question",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Kind:     format.KindQuestion,
		},
		Body: "ping",
	}
	data, _ := originalMsg.Marshal()
	if _, err := deliverToInboxesForTest(t, root, []string{"alice"}, originalID+".md", data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Simulate bob draining the reply: emit a drained receipt once it lands.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := os.ReadDir(filepath.Join(root, "agents", "bob", "inbox", "new"))
			if err == nil && len(entries) == 1 {
				msgID := strings.TrimSuffix(entries[0].Name(), ".md")
				r := receipt.New(msgID, "", "alice", "bob", receipt.StageDrained, "")
				_ = receipt.Emit(root, r)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	output, err := captureEnvStdout(t, func() error {
		return runReply([]string{
			"--me", "alice",
			"--root", root,
			"--id", originalID,
			"--body", "pong",
			"--wait-for", "drained",
			"--wait-timeout", "3s",
			"--json",
		})
	})
	<-done
	if err != nil {
		t.Fatalf("runReply --wait-for: %v (output: %s)", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, output)
	}
	wait, ok := result["wait"].(map[string]any)
	if !ok {
		t.Fatalf("expected wait object in result, got %v", result["wait"])
	}
	if wait["event"] != "matched" {
		t.Fatalf("wait.event = %v, want matched (output: %s)", wait["event"], output)
	}
}

func TestReplyWaitForRejectsInvalidStage(t *testing.T) {
	err := runReply([]string{
		"--me", "alice",
		"--root", t.TempDir(),
		"--id", "someid",
		"--body", "x",
		"--wait-for", "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "--wait-for") {
		t.Fatalf("expected --wait-for usage error, got %v", err)
	}
}

func TestWhoTextOutputShowsBaseRoot(t *testing.T) {
	baseRoot := t.TempDir()
	root := filepath.Join(baseRoot, "collab")
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root})
	})
	if err != nil {
		t.Fatalf("runWho: %v", err)
	}
	if !strings.Contains(output, "Base root: ") {
		t.Fatalf("who text output should include base root header, got:\n%s", output)
	}
	if !strings.Contains(output, baseRoot) {
		t.Fatalf("who text output should name %q, got:\n%s", baseRoot, output)
	}
}
