package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/resolve"
)

// setupFederationEnv creates two sessions under a base root and configures
// environment variables for cross-session testing. Returns (baseRoot, sessionA, sessionB).
func setupFederationEnv(t *testing.T) (string, string, string) {
	t.Helper()

	baseRoot := t.TempDir()
	sessionA := filepath.Join(baseRoot, "alpha")
	sessionB := filepath.Join(baseRoot, "beta")

	// Create session directories and agent mailboxes.
	for _, sess := range []string{sessionA, sessionB} {
		if err := fsq.EnsureRootDirs(sess); err != nil {
			t.Fatalf("EnsureRootDirs(%s): %v", sess, err)
		}
	}

	// Session A has alice and bob.
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(sessionA, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Session B has bob and charlie.
	for _, agent := range []string{"bob", "charlie"} {
		if err := fsq.EnsureAgentDirs(sessionB, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	return baseRoot, sessionA, sessionB
}

func TestFederation_CrossSessionSend(t *testing.T) {
	baseRoot, sessionA, sessionB := setupFederationEnv(t)

	// Clear env vars to avoid conflicts.
	t.Setenv("AM_ROOT", sessionA)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_ME", "alice")
	t.Setenv("AM_PROJECT", "")

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Alice in session alpha sends to bob@beta (cross-session).
	err := runSend([]string{
		"--me", "alice",
		"--root", sessionA,
		"--to", "bob@beta",
		"--subject", "Cross-session test",
		"--body", "Hello from alpha!",
		"--thread", "test/federation",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend (cross-session): %v", err)
	}

	// Parse JSON output.
	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, output)
	}

	// Verify federated metadata.
	if result["federated"] != true {
		t.Errorf("expected federated=true, got %v", result["federated"])
	}
	if result["scope"] != "cross-session" {
		t.Errorf("expected scope=cross-session, got %v", result["scope"])
	}

	// Verify message arrived in session B's bob inbox/new.
	bobInbox := fsq.AgentInboxNew(sessionB, "bob")
	entries, err := os.ReadDir(bobInbox)
	if err != nil {
		t.Fatalf("read bob inbox in session B: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in bob@beta inbox, got %d", len(entries))
	}

	// Parse the delivered message and verify Origin/Delivery fields.
	msgPath := filepath.Join(bobInbox, entries[0].Name())
	msg, err := format.ReadMessageFile(msgPath)
	if err != nil {
		t.Fatalf("read delivered message: %v", err)
	}

	if msg.Header.From != "alice" {
		t.Errorf("expected from=alice, got %s", msg.Header.From)
	}
	if msg.Body != "Hello from alpha!\n" {
		t.Errorf("expected body='Hello from alpha!\\n', got %q", msg.Body)
	}

	// Verify Origin is populated.
	if msg.Header.Origin == nil {
		t.Fatal("expected Origin to be populated")
	}
	if msg.Header.Origin.Agent != "alice" {
		t.Errorf("expected origin.agent=alice, got %s", msg.Header.Origin.Agent)
	}
	if msg.Header.Origin.Session != "alpha" {
		t.Errorf("expected origin.session=alpha, got %s", msg.Header.Origin.Session)
	}
	if msg.Header.Origin.ReplyTo != "alice@alpha" {
		t.Errorf("expected origin.reply_to=alice@alpha, got %s", msg.Header.Origin.ReplyTo)
	}

	// Verify Delivery is populated.
	if msg.Header.Delivery == nil {
		t.Fatal("expected Delivery to be populated")
	}
	if msg.Header.Delivery.Scope != "cross-session" {
		t.Errorf("expected delivery.scope=cross-session, got %s", msg.Header.Delivery.Scope)
	}
	if len(msg.Header.Delivery.RequestedTo) != 1 || msg.Header.Delivery.RequestedTo[0] != "bob@beta" {
		t.Errorf("expected delivery.requested_to=[bob@beta], got %v", msg.Header.Delivery.RequestedTo)
	}

	// Verify message was also copied to sender outbox.
	aliceOutbox := fsq.AgentOutboxSent(sessionA, "alice")
	outboxEntries, err := os.ReadDir(aliceOutbox)
	if err != nil {
		t.Fatalf("read alice outbox: %v", err)
	}
	if len(outboxEntries) != 1 {
		t.Errorf("expected 1 message in alice outbox, got %d", len(outboxEntries))
	}
}

func TestFederation_CrossSessionReply(t *testing.T) {
	baseRoot, sessionA, sessionB := setupFederationEnv(t)

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_PROJECT", "")

	// Step 1: Deliver a message from alice@alpha to bob@beta with Origin.ReplyTo.
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    "alice",
			To:      []string{"bob"},
			Thread:  "test/fed-reply",
			Subject: "Federation reply test",
			Created: now.UTC().Format(time.RFC3339Nano),
			Kind:    format.KindQuestion,
			Origin: &format.Origin{
				Agent:   "alice",
				Session: "alpha",
				ReplyTo: "alice@alpha",
			},
			Delivery: &format.Delivery{
				Scope: "cross-session",
			},
		},
		Body: "Question from alpha session",
	}
	data, err := originalMsg.Marshal()
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	filename := originalID + ".md"
	// Deliver to bob in session B.
	if _, err := fsq.DeliverToInbox(sessionB, "bob", filename, data); err != nil {
		t.Fatalf("deliver original to bob@beta: %v", err)
	}

	// Step 2: Bob in session B replies using the Origin.ReplyTo address.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = runReply([]string{
		"--me", "bob",
		"--root", sessionB,
		"--id", originalID,
		"--body", "Answer from beta session",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply (federated): %v", err)
	}

	// Parse JSON output.
	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, output)
	}

	// Verify federated reply metadata.
	if result["federated"] != true {
		t.Errorf("expected federated=true, got %v", result["federated"])
	}
	if result["scope"] != "cross-session" {
		t.Errorf("expected scope=cross-session, got %v", result["scope"])
	}
	if result["in_reply_to"] != originalID {
		t.Errorf("expected in_reply_to=%s, got %v", originalID, result["in_reply_to"])
	}

	// Verify the reply arrived in alice's inbox in session A.
	aliceInbox := fsq.AgentInboxNew(sessionA, "alice")
	entries, err := os.ReadDir(aliceInbox)
	if err != nil {
		t.Fatalf("read alice inbox in session A: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in alice@alpha inbox, got %d", len(entries))
	}

	// Parse and verify the reply message.
	replyPath := filepath.Join(aliceInbox, entries[0].Name())
	replyMsg, err := format.ReadMessageFile(replyPath)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}

	if replyMsg.Header.From != "bob" {
		t.Errorf("expected from=bob, got %s", replyMsg.Header.From)
	}
	if replyMsg.Header.Kind != format.KindAnswer {
		t.Errorf("expected kind=answer (auto from question), got %s", replyMsg.Header.Kind)
	}
	if replyMsg.Header.Thread != "test/fed-reply" {
		t.Errorf("expected thread=test/fed-reply, got %s", replyMsg.Header.Thread)
	}
	if len(replyMsg.Header.Refs) != 1 || replyMsg.Header.Refs[0] != originalID {
		t.Errorf("expected refs=[%s], got %v", originalID, replyMsg.Header.Refs)
	}

	// Verify Origin on the reply.
	if replyMsg.Header.Origin == nil {
		t.Fatal("expected Origin on reply")
	}
	if replyMsg.Header.Origin.Agent != "bob" {
		t.Errorf("expected origin.agent=bob, got %s", replyMsg.Header.Origin.Agent)
	}
	if replyMsg.Header.Origin.Session != "beta" {
		t.Errorf("expected origin.session=beta, got %s", replyMsg.Header.Origin.Session)
	}

	// Verify Delivery on the reply.
	if replyMsg.Header.Delivery == nil {
		t.Fatal("expected Delivery on reply")
	}
	if replyMsg.Header.Delivery.Scope != "cross-session" {
		t.Errorf("expected delivery.scope=cross-session, got %s", replyMsg.Header.Delivery.Scope)
	}
}

func TestFederation_LocalSendUnchanged(t *testing.T) {
	// Verify that bare-handle sends still use the existing local path
	// and do NOT populate Origin/Delivery fields.
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_PROJECT", "")

	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSend([]string{
		"--me", "alice",
		"--root", root,
		"--to", "bob",
		"--subject", "Local test",
		"--body", "Hello locally!",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend (local): %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	if err := json.Unmarshal(buf[:n], &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	// Local sends should NOT have "federated" key.
	if _, hasFederated := result["federated"]; hasFederated {
		t.Errorf("local send should not have federated key, got %v", result["federated"])
	}

	// Verify message delivered to bob.
	bobInbox := fsq.AgentInboxNew(root, "bob")
	entries, err := os.ReadDir(bobInbox)
	if err != nil {
		t.Fatalf("read bob inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in bob inbox, got %d", len(entries))
	}

	// Verify the message does NOT have Origin or Delivery.
	msgPath := filepath.Join(bobInbox, entries[0].Name())
	msg, err := format.ReadMessageFile(msgPath)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Header.Origin != nil {
		t.Errorf("local send should not have Origin, got %+v", msg.Header.Origin)
	}
	if msg.Header.Delivery != nil {
		t.Errorf("local send should not have Delivery, got %+v", msg.Header.Delivery)
	}
}

func TestFederation_LocalReplyUnchanged(t *testing.T) {
	// Verify that replying to a message without Origin.ReplyTo uses the
	// existing local path and does NOT populate federation fields.
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_PROJECT", "")

	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Create an original message without Origin (legacy).
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Local question",
			Created: now.UTC().Format(time.RFC3339Nano),
			Kind:    format.KindQuestion,
		},
		Body: "How does this work?",
	}
	data, _ := originalMsg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", originalID+".md", data); err != nil {
		t.Fatalf("deliver original: %v", err)
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runReply([]string{
		"--me", "alice",
		"--root", root,
		"--id", originalID,
		"--body", "Like this!",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply (local): %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	if err := json.Unmarshal(buf[:n], &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	// Local replies should NOT have "federated" key.
	if _, hasFederated := result["federated"]; hasFederated {
		t.Errorf("local reply should not have federated key")
	}

	// Verify reply delivered to bob.
	bobInbox := fsq.AgentInboxNew(root, "bob")
	entries, err := os.ReadDir(bobInbox)
	if err != nil {
		t.Fatalf("read bob inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in bob inbox, got %d", len(entries))
	}

	// Verify the reply does NOT have Origin or Delivery.
	replyPath := filepath.Join(bobInbox, entries[0].Name())
	replyMsg, err := format.ReadMessageFile(replyPath)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if replyMsg.Header.Origin != nil {
		t.Errorf("local reply should not have Origin, got %+v", replyMsg.Header.Origin)
	}
	if replyMsg.Header.Delivery != nil {
		t.Errorf("local reply should not have Delivery, got %+v", replyMsg.Header.Delivery)
	}
	if replyMsg.Header.Kind != format.KindAnswer {
		t.Errorf("expected kind=answer, got %s", replyMsg.Header.Kind)
	}
}

func TestFederation_SameSessionQualifiedSend(t *testing.T) {
	// Verify that sending to agent@<current-session> delivers via federation
	// path but uses "local" scope since the SessionRoot matches.
	baseRoot, sessionA, _ := setupFederationEnv(t)

	t.Setenv("AM_ROOT", sessionA)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_PROJECT", "")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// alice@alpha sends to bob@alpha (same session, but qualified).
	err := runSend([]string{
		"--me", "alice",
		"--root", sessionA,
		"--to", "bob@alpha",
		"--subject", "Same session qualified",
		"--body", "Hello in same session!",
		"--thread", "test/same-session",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend (same session qualified): %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, output)
	}

	if result["federated"] != true {
		t.Errorf("expected federated=true for qualified address")
	}
	// Same-session should be scope "local" since SessionRoot matches.
	if result["scope"] != "local" {
		t.Errorf("expected scope=local for same-session qualified, got %v", result["scope"])
	}

	// Verify delivery.
	bobInbox := fsq.AgentInboxNew(sessionA, "bob")
	entries, _ := os.ReadDir(bobInbox)
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in bob@alpha inbox, got %d", len(entries))
	}

	msgPath := filepath.Join(bobInbox, entries[0].Name())
	msg, _ := format.ReadMessageFile(msgPath)
	if msg.Header.Origin == nil {
		t.Fatal("expected Origin on qualified send")
	}
	if msg.Header.Origin.Agent != "alice" {
		t.Errorf("expected origin.agent=alice, got %s", msg.Header.Origin.Agent)
	}
}

func TestFederation_HasQualifiedRecipient(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"bob", false},
		{"alice,bob", false},
		{"bob@beta", true},
		{"#all", true},
		{"alice,bob@beta", true},
		{"claude@infra:auth", true},
		{"alice@session/auth", true},
		{"", false},
	}
	for _, tt := range tests {
		got := hasQualifiedRecipient(tt.input)
		if got != tt.want {
			t.Errorf("hasQualifiedRecipient(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFederation_FormatResolvedTarget(t *testing.T) {
	tests := []struct {
		name   string
		target resolve.Target
		want   string
	}{
		{
			name:   "bare agent",
			target: resolve.Target{Agent: "bob"},
			want:   "bob",
		},
		{
			name:   "agent with session",
			target: resolve.Target{Agent: "bob", Session: "beta"},
			want:   "bob@beta",
		},
		{
			name:   "agent with project and session",
			target: resolve.Target{Agent: "bob", Session: "collab", Project: "infra"},
			want:   "bob@infra:collab",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatResolvedTarget(tt.target)
			if got != tt.want {
				t.Errorf("formatResolvedTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}
