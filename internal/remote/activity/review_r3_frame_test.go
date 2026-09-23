package activity

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
)

// Codex #868 r3 2026-09-23T09-22-12.363Z_pid77460_2fd2e2c6: a tool result
// that fits Desktop replacement must still fit the relay outbound frame.
func TestReviewR3ToolResultFitsRelayFrame(t *testing.T) {
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
	if err := s.AcceptClaude(context.Background(), string(line)); err != nil {
		t.Fatal(err)
	}
}
