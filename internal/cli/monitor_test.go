package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestMonitor_ExistingMessages(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	// Initialize mailbox
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a test message with co-op fields
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     "bob",
			To:       []string{agent},
			Thread:   "p2p/alice__bob",
			Subject:  "Test review",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityUrgent,
			Kind:     format.KindReviewRequest,
			Labels:   []string{"test", "urgent"},
		},
		Body: "Please review this.",
	}
	data, _ := msg.Marshal()
	filename := id + ".md"
	if _, err := fsq.DeliverToInboxes(root, []string{agent}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor with --once (should drain existing and exit)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--once",
		"--timeout", "1s",
		"--include-body",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify result
	if result.Event != "messages" {
		t.Errorf("expected event=messages, got %s", result.Event)
	}
	if result.WatchEvent != "existing" {
		t.Errorf("expected watch_event=existing, got %s", result.WatchEvent)
	}
	if result.Count != 1 {
		t.Errorf("expected count=1, got %d", result.Count)
	}
	if len(result.Drained) != 1 {
		t.Fatalf("expected 1 drained item, got %d", len(result.Drained))
	}

	item := result.Drained[0]
	if item.Priority != format.PriorityUrgent {
		t.Errorf("expected priority=urgent, got %s", item.Priority)
	}
	if item.Kind != format.KindReviewRequest {
		t.Errorf("expected kind=review_request, got %s", item.Kind)
	}
	if len(item.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(item.Labels))
	}
	if item.Body != "Please review this.\n" {
		t.Errorf("unexpected body: %q", item.Body)
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
	if _, err := os.Stat(curPath); os.IsNotExist(err) {
		t.Error("message not moved to cur")
	}
}

func TestMonitor_Timeout(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor with short timeout (no messages)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--once",
		"--timeout", "100ms",
		"--poll",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify timeout result
	if result.Event != "timeout" {
		t.Errorf("expected event=timeout, got %s", result.Event)
	}
	if result.Count != 0 {
		t.Errorf("expected count=0, got %d", result.Count)
	}
}

func TestMonitor_PriorityInOutput(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages with different priorities
	priorities := []string{format.PriorityLow, format.PriorityNormal, format.PriorityUrgent}
	for i, p := range priorities {
		now := time.Now().Add(time.Duration(i) * time.Second)
		id, _ := format.NewMessageID(now)
		msg := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       id,
				From:     "bob",
				To:       []string{agent},
				Thread:   "p2p/alice__bob",
				Subject:  "Priority " + p,
				Created:  now.UTC().Format(time.RFC3339Nano),
				Priority: p,
			},
			Body: "Test",
		}
		data, _ := msg.Marshal()
		_, _ = fsq.DeliverToInboxes(root, []string{agent}, id+".md", data)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--once",
		"--timeout", "1s",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	if result.Count != 3 {
		t.Errorf("expected 3 messages, got %d", result.Count)
	}

	// Verify all priorities present
	foundPriorities := make(map[string]bool)
	for _, item := range result.Drained {
		foundPriorities[item.Priority] = true
	}
	for _, p := range priorities {
		if !foundPriorities[p] {
			t.Errorf("priority %s not found in output", p)
		}
	}
}
