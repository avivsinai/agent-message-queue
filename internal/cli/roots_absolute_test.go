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
