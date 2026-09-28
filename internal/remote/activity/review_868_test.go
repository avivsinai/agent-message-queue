package activity

import (
	"context"
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Codex #868 review 2026-09-23T07-04-12.081Z_pid46637_0449f9e4: a tool_call
// needs a title, and tool content is an array. The text-message codec is
// the wrong shape.
func TestReviewToolCallHasACPShape(t *testing.T) {
	got := claudeUpdates(t, `{"type":"assistant","uuid":"a1","sessionId":"thread-1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"private"}}]}}`)
	if len(got) != 1 {
		t.Fatalf("updates=%d", len(got))
	}
	if title, ok := got[0]["title"].(string); !ok || title == "" {
		t.Errorf("tool_call lacks required ACP title: %#v", got[0])
	}
	if content, ok := got[0]["content"]; ok {
		if _, ok := content.([]any); !ok {
			t.Errorf("ACP tool content must be an array, got %T", content)
		}
	}
}

// Codex #868 review 2026-09-23T07-04-12.081Z_pid46637_0449f9e4: is_error
// stays failed. Desktop treats a missing status as completed.
func TestReviewToolErrorRemainsFailed(t *testing.T) {
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
	if err := s.AcceptClaude(context.Background(), line); err != nil {
		t.Fatal(err)
	}
	return updates
}

// Code review 2026-09-28 (611.40): a prompt sent from Buzz reaches a Claude
// session inside the cross-session envelope and harness prose, and the
// activity view mirrored that XML instead of the text the owner typed.
func TestReviewBuzzPromptMirrorsTypedText(t *testing.T) {
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
