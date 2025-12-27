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
)

func TestReply_Basic(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	// Initialize mailboxes
	for _, agent := range []string{alice, bob} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Create an original message from Bob to Alice
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          originalID,
			From:        bob,
			To:          []string{alice},
			Thread:      "p2p/alice__bob",
			Subject:     "Question about code",
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: true,
			Priority:    format.PriorityNormal,
			Kind:        format.KindQuestion,
		},
		Body: "How does the parser work?",
	}
	data, _ := originalMsg.Marshal()
	filename := originalID + ".md"
	if _, err := fsq.DeliverToInboxes(root, []string{alice}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Alice replies
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "It parses JSON frontmatter.",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply: %v", err)
	}

	// Parse output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify reply metadata
	if result["to"] != bob {
		t.Errorf("expected to=bob, got %v", result["to"])
	}
	if result["thread"] != "p2p/alice__bob" {
		t.Errorf("expected thread=p2p/alice__bob, got %v", result["thread"])
	}
	if result["in_reply_to"] != originalID {
		t.Errorf("expected in_reply_to=%s, got %v", originalID, result["in_reply_to"])
	}
	subject := result["subject"].(string)
	if !strings.HasPrefix(subject, "Re:") {
		t.Errorf("expected subject to start with 'Re:', got %s", subject)
	}

	// Verify message delivered to Bob
	bobInbox := fsq.AgentInboxNew(root, bob)
	entries, _ := os.ReadDir(bobInbox)
	if len(entries) != 1 {
		t.Errorf("expected 1 message in Bob's inbox, got %d", len(entries))
	}

	// Read the reply message
	if len(entries) > 0 {
		replyPath := filepath.Join(bobInbox, entries[0].Name())
		replyMsg, err := format.ReadMessageFile(replyPath)
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}

		// Verify refs contains original ID
		if len(replyMsg.Header.Refs) != 1 || replyMsg.Header.Refs[0] != originalID {
			t.Errorf("expected refs=[%s], got %v", originalID, replyMsg.Header.Refs)
		}

		// Verify kind auto-set to review_response for question
		if replyMsg.Header.Kind != format.KindReviewResponse {
			t.Errorf("expected kind=review_response (auto-set from question), got %s", replyMsg.Header.Kind)
		}
	}
}

func TestReply_ReviewRequest(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	for _, agent := range []string{alice, bob} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Create a review request from Bob to Alice
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       originalID,
			From:     bob,
			To:       []string{alice},
			Thread:   "p2p/alice__bob",
			Subject:  "Review: New feature",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Kind:     format.KindReviewRequest,
			Labels:   []string{"feature", "ui"},
		},
		Body: "Please review this feature.",
	}
	data, _ := originalMsg.Marshal()
	_, _ = fsq.DeliverToInboxes(root, []string{alice}, originalID+".md", data)

	// Alice replies with explicit kind
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "LGTM!",
		"--priority", "normal",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply: %v", err)
	}

	var buf [4096]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	_ = json.Unmarshal(buf[:n], &result)

	// Verify Bob received the reply
	bobInbox := fsq.AgentInboxNew(root, bob)
	entries, _ := os.ReadDir(bobInbox)
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in Bob's inbox, got %d", len(entries))
	}

	replyPath := filepath.Join(bobInbox, entries[0].Name())
	replyMsg, _ := format.ReadMessageFile(replyPath)

	// Kind should be auto-set to review_response
	if replyMsg.Header.Kind != format.KindReviewResponse {
		t.Errorf("expected kind=review_response, got %s", replyMsg.Header.Kind)
	}
}

func TestReply_PreservesThread(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	for _, agent := range []string{alice, bob} {
		_ = fsq.EnsureAgentDirs(root, agent)
	}

	// Create message with custom thread
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    bob,
			To:      []string{alice},
			Thread:  "project/feature-123",
			Subject: "Feature discussion",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "Let's discuss.",
	}
	data, _ := originalMsg.Marshal()
	_, _ = fsq.DeliverToInboxes(root, []string{alice}, originalID+".md", data)

	// Reply
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	_ = runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "Sounds good!",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	var buf [4096]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	_ = json.Unmarshal(buf[:n], &result)

	// Thread should be preserved
	if result["thread"] != "project/feature-123" {
		t.Errorf("expected thread=project/feature-123, got %v", result["thread"])
	}
}
