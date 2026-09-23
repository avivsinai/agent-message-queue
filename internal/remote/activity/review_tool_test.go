package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex #868 r2 2026-09-23T08-44-02.855Z_pid79326_2ce7f4df: Desktop replaces
// the tool result on each update, so a split result keeps only the tail.
// Codex #868 r3 2026-09-23T09-22-12.363Z_pid77460_2fd2e2c6: that one update
// must also fit the relay frame, so the displayed text is a prefix.
func TestReviewLargeToolResultSurvivesReplacementUpdates(t *testing.T) {
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
