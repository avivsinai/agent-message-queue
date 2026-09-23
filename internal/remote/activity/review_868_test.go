package activity

import (
	"context"
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
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
