package mirror

import (
	"encoding/json"
	"strings"
	"testing"
)

// rollout is one synthetic Codex rollout event line, n seconds after base.
func rollout(n int, payload map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"timestamp": at(n), "type": "event_msg", "payload": payload})
	return string(raw)
}

func codexItem(n int, turn string, item map[string]any) string {
	return rollout(n, map[string]any{"type": "item_completed", "thread_id": "th", "turn_id": turn, "item": item})
}

// A finished Codex turn makes one post: its prompt, its tool count and its
// last answer, never a command, its output or a file change. A turn the
// Buzz carrier sent (its client_id carried) shows the first two lines only.
func TestFoldCodexRendersFinishedTurns(t *testing.T) {
	const clientID = "amqr1_local_codex-1_00000000-0000-4000-8000-000000000001"
	carried := func(id string) bool { return id == CodexCarriedID(clientID) }
	turn := func(user map[string]any) []string {
		return []string{
			`{"timestamp":"` + at(0) + `","type":"session_meta","payload":{"id":"th","history_mode":"paginated","base_instructions":{"text":"SECRET"}}}`,
			rollout(0, map[string]any{"type": "task_started", "turn_id": "t1", "turn_attribution": map[string]any{"turn_id": "t1", "turn_trigger": "user"}}),
			codexItem(0, "t1", user),
			codexItem(1, "t1", map[string]any{"type": "AgentMessage", "id": "a1", "phase": "commentary", "content": []any{map[string]any{"type": "Text", "text": "Running them."}}}),
			codexItem(4, "t1", map[string]any{"type": "CommandExecution", "id": "c1", "source": "agent", "command": []any{"SECRET"}, "aggregated_output": "SECRET", "status": "completed"}),
			codexItem(5, "t1", map[string]any{"type": "CommandExecution", "id": "c2", "source": "user_shell", "command": []any{"SECRET"}, "status": "completed"}),
			codexItem(6, "t1", map[string]any{"type": "FileChange", "id": "f1", "changes": map[string]any{"a.go": map[string]any{"diff": "SECRET"}}}),
			codexItem(9, "t1", map[string]any{"type": "CommandExecution", "id": "c3", "source": "agent", "command": []any{"SECRET"}, "status": "completed"}),
			codexItem(11, "t1", map[string]any{"type": "AgentMessage", "id": "a2", "phase": "final_answer", "content": []any{map[string]any{"type": "Text", "text": "All green."}}}),
			rollout(12, map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": "All green.", "error": nil}),
		}
	}
	rows := []struct {
		name string
		user map[string]any
		post string
	}{{
		name: "terminal prompt",
		user: map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": "run the tests\nand fix them"}}},
		post: "▶ run the tests\n✓ 3 tools (command ×2, edit) · 12s\nAll green.",
	}, {
		name: "Buzz prompt",
		user: map[string]any{"type": "UserMessage", "id": "u1", "client_id": clientID, "content": []any{map[string]any{"type": "text", "text": "run the tests"}}},
		post: "▶ Buzz\n✓ 3 tools (command ×2, edit) · 12s",
	}}
	src := SourceFor("codex")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var st State
			var events []Event
			for _, l := range turn(row.user) {
				var evs []Event
				st, evs = src.Fold(st, l, carried)
				events = append(events, evs...)
			}
			if len(events) != 1 || events[0].Kind != Ended {
				t.Fatalf("events = %+v, want one Ended", events)
			}
			post := Render(events[0].Turn)
			if post != row.post || strings.Contains(post, "SECRET") {
				t.Fatalf("post =\n%s\nwant\n%s", post, row.post)
			}
		})
	}
}
