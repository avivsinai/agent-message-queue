package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex #868 r2 2026-09-23T08-44-02.855Z_pid79326_2ce7f4df: Desktop replaces
// the tool result on each update, so a split result keeps only the tail.
func TestReviewLargeToolResultSurvivesReplacementUpdates(t *testing.T) {
	text := strings.Repeat("result", 12000)
	line, _ := json.Marshal(map[string]any{"type": "user", "uuid": "r1", "sessionId": "thread-1", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": text}}}})
	updates := claudeUpdates(t, string(line))
	var displayed string
	// Buzz agentSessionTranscript.ts upsertTool uses result || existing.result.
	for _, update := range updates {
		content, ok := update["content"].([]any)
		if !ok {
			continue
		}
		var result strings.Builder
		for _, item := range content {
			block := item.(map[string]any)["content"].(map[string]any)
			result.WriteString(block["text"].(string))
		}
		if result.Len() > 0 {
			displayed = result.String()
		}
	}
	if displayed != text {
		t.Fatalf("%d updates display only %d of %d bytes", len(updates), len(displayed), len(text))
	}
}
