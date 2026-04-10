package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// setupCoopFixture creates a minimal coop session directory structure
// with the given agents and optional messages in inbox/new.
func setupCoopFixture(t *testing.T, session string, agents []string, messagesFor map[string][]string) string {
	t.Helper()
	base := t.TempDir()
	sessDir := filepath.Join(base, session)
	for _, agent := range agents {
		inbox := filepath.Join(sessDir, "agents", agent, "inbox")
		if err := os.MkdirAll(filepath.Join(inbox, "new"), 0o700); err != nil {
			t.Fatalf("mkdir inbox/new: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(inbox, "cur"), 0o700); err != nil {
			t.Fatalf("mkdir inbox/cur: %v", err)
		}
	}
	// Write messages
	for agent, msgs := range messagesFor {
		newDir := filepath.Join(sessDir, "agents", agent, "inbox", "new")
		for _, msg := range msgs {
			if err := os.WriteFile(filepath.Join(newDir, msg), []byte("test"), 0o600); err != nil {
				t.Fatalf("write msg: %v", err)
			}
		}
	}
	return sessDir
}

func TestBuildPreamble_Basic(t *testing.T) {
	peers := []claudePeerInfo{
		{Handle: "codex", Active: true},
	}
	got := buildPreamble("claude", "collab", "myproject", peers, 0)

	if !strings.Contains(got, "AMQ coop active: me=claude") {
		t.Fatalf("preamble missing identity: %s", got)
	}
	if !strings.Contains(got, "session=collab") {
		t.Fatalf("preamble missing session: %s", got)
	}
	if !strings.Contains(got, "project=myproject") {
		t.Fatalf("preamble missing project: %s", got)
	}
	if !strings.Contains(got, "codex(active)") {
		t.Fatalf("preamble missing active peer: %s", got)
	}
	if !strings.Contains(got, "amq send/reply/drain") {
		t.Fatalf("preamble missing routing instructions: %s", got)
	}
	if strings.Contains(got, "Inbox:") {
		t.Fatalf("preamble should not mention inbox when 0 messages: %s", got)
	}
}

func TestBuildPreamble_WithMessages(t *testing.T) {
	got := buildPreamble("claude", "collab", "", nil, 3)

	if !strings.Contains(got, "Inbox: 3 unread") {
		t.Fatalf("preamble missing inbox count: %s", got)
	}
	if !strings.Contains(got, "amq drain --me claude") {
		t.Fatalf("preamble missing drain hint: %s", got)
	}
}

func TestBuildPreamble_NoPeers(t *testing.T) {
	got := buildPreamble("claude", "collab", "", nil, 0)

	if strings.Contains(got, "peers=") {
		t.Fatalf("preamble should not contain peers= when none: %s", got)
	}
}

func TestBuildPreamble_StalePeer(t *testing.T) {
	peers := []claudePeerInfo{
		{Handle: "codex", Active: false},
	}
	got := buildPreamble("claude", "collab", "", peers, 0)

	if !strings.Contains(got, "codex(stale)") {
		t.Fatalf("preamble missing stale peer: %s", got)
	}
}

func TestDiscoverPeers(t *testing.T) {
	sessDir := setupCoopFixture(t, "collab", []string{"claude", "codex", "gemini"}, nil)

	// Set codex as active
	now := time.Now()
	p := presence.New("codex", "active", "", now)
	if err := presence.Write(sessDir, p); err != nil {
		t.Fatalf("write presence: %v", err)
	}

	peers := discoverPeers(sessDir, "claude")

	if len(peers) != 2 {
		t.Fatalf("expected 2 peers (codex, gemini), got %d: %v", len(peers), peers)
	}

	// Find codex in peers
	var foundCodex, foundGemini bool
	for _, p := range peers {
		if p.Handle == "codex" {
			foundCodex = true
			if !p.Active {
				t.Fatal("codex should be active")
			}
		}
		if p.Handle == "gemini" {
			foundGemini = true
			if p.Active {
				t.Fatal("gemini should be stale (no presence)")
			}
		}
	}
	if !foundCodex || !foundGemini {
		t.Fatalf("missing expected peers: codex=%v gemini=%v", foundCodex, foundGemini)
	}
}

func TestDiscoverPeers_ExcludesSelf(t *testing.T) {
	sessDir := setupCoopFixture(t, "collab", []string{"claude", "codex"}, nil)

	peers := discoverPeers(sessDir, "claude")

	for _, p := range peers {
		if p.Handle == "claude" {
			t.Fatal("peers should not include self")
		}
	}
}

func TestCountNewMessages(t *testing.T) {
	sessDir := setupCoopFixture(t, "collab", []string{"claude"}, map[string][]string{
		"claude": {"msg_001.md", "msg_002.md", "msg_003.md"},
	})

	count := countNewMessages(sessDir, "claude")
	if count != 3 {
		t.Fatalf("expected 3 new messages, got %d", count)
	}
}

func TestCountNewMessages_Empty(t *testing.T) {
	sessDir := setupCoopFixture(t, "collab", []string{"claude"}, nil)

	count := countNewMessages(sessDir, "claude")
	if count != 0 {
		t.Fatalf("expected 0 new messages, got %d", count)
	}
}

func TestCountNewMessages_IgnoresNonMd(t *testing.T) {
	sessDir := setupCoopFixture(t, "collab", []string{"claude"}, map[string][]string{
		"claude": {"msg_001.md", "not_a_message.txt"},
	})

	count := countNewMessages(sessDir, "claude")
	if count != 1 {
		t.Fatalf("expected 1 new message (.md only), got %d", count)
	}
}

func TestMarshalClaudeHookOutput(t *testing.T) {
	data, err := marshalClaudeHookOutput("test preamble", "AMQ: 1 peer")
	if err != nil {
		t.Fatalf("marshalClaudeHookOutput: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	hso, ok := parsed["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if hso["additionalContext"] != "test preamble" {
		t.Fatalf("additionalContext = %v, want %q", hso["additionalContext"], "test preamble")
	}
	if parsed["systemMessage"] != "AMQ: 1 peer" {
		t.Fatalf("systemMessage = %v, want %q", parsed["systemMessage"], "AMQ: 1 peer")
	}
}

func TestMarshalClaudeHookOutput_NoBanner(t *testing.T) {
	data, err := marshalClaudeHookOutput("preamble only", "")
	if err != nil {
		t.Fatalf("marshalClaudeHookOutput: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if _, exists := parsed["systemMessage"]; exists {
		t.Fatal("systemMessage should not be present when banner is empty")
	}
}

func TestRunClaudeContext_Help(t *testing.T) {
	err := runClaudeContext([]string{"--help"})
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
}

func TestRunIntegrationClaude_Help(t *testing.T) {
	err := runIntegrationClaude([]string{"--help"})
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
}

func TestRunIntegrationClaude_UnknownSubcommand(t *testing.T) {
	err := runIntegrationClaude([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error should mention unknown: %v", err)
	}
}

