package activity

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Regression: #868, is_error stays failed. Desktop treats a missing status as completed.
func TestToolErrorRemainsFailed(t *testing.T) {
	got := claudeUpdates(t, `{"type":"user","uuid":"r1","sessionId":"thread-1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"permission denied"}]}}`)
	if len(got) != 1 {
		t.Fatalf("updates=%d", len(got))
	}
	if got[0]["status"] != "failed" {
		t.Fatalf("observed failed tool result loses status: %#v", got[0])
	}
}

func claudeUpdates(t *testing.T, line string) []map[string]any {
	t.Helper()
	body, owner := nostr.Generate(), nostr.Generate()
	key, err := nip44.GenerateConversationKey(body.Public(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var updates []map[string]any
	s := testSink(body, owner, func(_ context.Context, e nostr.Event) error {
		plain, err := nip44.Decrypt(e.Content, key)
		if err != nil {
			return err
		}
		var o observerJSON
		if err := json.Unmarshal([]byte(plain), &o); err != nil {
			return err
		}
		if o.Kind != "acp_read" {
			return nil
		}
		var p struct {
			Params struct {
				Update map[string]any `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			return err
		}
		updates = append(updates, p.Params.Update)
		return nil
	})
	if err := deliverClaude(context.Background(), s, line); err != nil {
		t.Fatal(err)
	}
	return updates
}

// Regression: agent-message-queue-611.40, a prompt sent from Buzz reaches a Claude
// session inside the cross-session envelope and harness prose, and the
// activity view mirrored that XML instead of the text the owner typed.
func TestBuzzPromptMirrorsTypedText(t *testing.T) {
	typed := "fix the flaky <test> & report"
	env, err := claude.BuildCrossSessionEnvelope("amq-target", "amq", typed, "", "amq-remote", "code")
	if err != nil {
		t.Fatal(err)
	}
	// The idle-target delivery shape: the harness wraps the envelope.
	line, err := json.Marshal(map[string]any{
		"type": "user", "uuid": "u1", "sessionId": "thread-1", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "Another Claude session sent a message: " + env + "  This came from another Claude session — not typed by your user."},
		"origin":  map[string]any{"kind": "peer", "from": "unknown", "msg_id": "m1", "name": "amq-remote"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := claudeUpdates(t, string(line))
	if len(got) != 1 || got[0]["sessionUpdate"] != "user_message_chunk" {
		t.Fatalf("updates = %#v", got)
	}
	if c, _ := got[0]["content"].(map[string]any); c["text"] != typed {
		t.Fatalf("mirrored prompt = %#v, want the typed text", c["text"])
	}
}

// Regression: #868, Desktop replaces the tool result on each update, so a
// split result keeps only the tail. That one update must also fit the relay
// frame, so the displayed text is a prefix.
func TestLargeToolResultKeepsPrefixAcrossReplacementUpdates(t *testing.T) {
	text := strings.Repeat("result", 12000)
	line, _ := json.Marshal(map[string]any{"type": "user", "uuid": "r1", "sessionId": "thread-1", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": text}}}})
	updates := claudeUpdates(t, string(line))
	var displayed string
	var withContent int
	// Buzz agentSessionTranscript.ts upsertTool uses result || existing.result.
	for _, update := range updates {
		content, ok := update["content"].([]any)
		if !ok {
			continue
		}
		withContent++
		var result strings.Builder
		for _, item := range content {
			block := item.(map[string]any)["content"].(map[string]any)
			result.WriteString(block["text"].(string))
		}
		if result.Len() > 0 {
			displayed = result.String()
		}
	}
	prefix, ok := strings.CutSuffix(displayed, toolResultBound)
	if withContent != 1 || !ok || !strings.HasPrefix(text, prefix) || prefix == "" || len(displayed) >= len(text) {
		t.Fatalf("%d content updates display %d of %d bytes", withContent, len(displayed), len(text))
	}
}

// Regression: #868, a tool result
// that fits Desktop replacement must still fit the relay outbound frame.
func TestToolResultFitsRelayFrame(t *testing.T) {
	line, err := json.Marshal(map[string]any{"type": "user", "uuid": "r1", "sessionId": "thread-1", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": strings.Repeat("result", 12000)}}}})
	if err != nil {
		t.Fatal(err)
	}
	s := testSink(nostr.Generate(), nostr.Generate(), func(_ context.Context, e nostr.Event) error {
		n, err := frameLen(e)
		if err != nil {
			return err
		}
		if n > relay.DefaultMaxOutbound {
			t.Errorf("tool activity EVENT is %d bytes; real relay publish limit is %d", n, relay.DefaultMaxOutbound)
		}
		return nil
	})
	defer s.Close()
	if err := deliverClaude(context.Background(), s, string(line)); err != nil {
		t.Fatal(err)
	}
}
