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
			Schema:   format.CurrentSchema,
			ID:       originalID,
			From:     bob,
			To:       []string{alice},
			Thread:   "p2p/alice__bob",
			Subject:  "Question about code",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Kind:     format.KindQuestion,
		},
		Body: "How does the parser work?",
	}
	data, _ := originalMsg.Marshal()
	filename := originalID + ".md"
	if _, err := deliverToInboxesForTest(t, root, []string{alice}, filename, data); err != nil {
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
	toSlice, ok := result["to"].([]any)
	if !ok || len(toSlice) != 1 || toSlice[0] != bob {
		t.Errorf("expected to=[bob], got %v", result["to"])
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

		// Verify kind auto-set to answer for question
		if replyMsg.Header.Kind != format.KindAnswer {
			t.Errorf("expected kind=answer (auto-set from question), got %s", replyMsg.Header.Kind)
		}
	}
}
