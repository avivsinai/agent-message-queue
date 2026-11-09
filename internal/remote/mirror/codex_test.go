package mirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// rollout is one synthetic Codex rollout event line, n seconds after base.
func rollout(n int, payload map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"timestamp": at(n), "type": "event_msg", "payload": payload})
	return string(raw)
}

func codexItem(n int, turn string, item map[string]any) string {
	return rollout(n, map[string]any{"type": "item_completed", "thread_id": "th", "turn_id": turn, "item": item})
}

func codexPrompt(n int, text string, extra map[string]any) string {
	item := map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": text}}}
	for k, v := range extra {
		item[k] = v
	}
	return codexItem(n, "t1", item)
}

// A finished Codex turn makes one post: its prompt, its tool count and its
// last answer, never a command, its output or a file change. A turn the
// Buzz carrier sent (its client_id carried) names Buzz as its cause.
func TestFoldCodexRendersFinishedTurns(t *testing.T) {
	const clientID = "amqr1_local_codex-1_00000000-0000-4000-8000-000000000001"
	carried := func(id string) bool { return id == CodexCarriedID(clientID) }
	start := rollout(0, map[string]any{"type": "task_started", "turn_id": "t1", "turn_attribution": map[string]any{"turn_id": "t1", "turn_trigger": "user"}})
	turn := func(user string) []string {
		return []string{
			`{"timestamp":"` + at(0) + `","type":"session_meta","payload":{"id":"th","history_mode":"paginated","base_instructions":{"text":"SECRET"}}}`,
			start,
			user,
			codexItem(1, "t1", map[string]any{"type": "AgentMessage", "id": "a1", "phase": "commentary", "content": []any{map[string]any{"type": "Text", "text": "Running them."}}}),
			codexItem(4, "t1", map[string]any{"type": "CommandExecution", "id": "c1", "source": "agent", "command": []any{"SECRET"}, "aggregated_output": "SECRET", "status": "completed"}),
			codexItem(5, "t1", map[string]any{"type": "CommandExecution", "id": "c2", "source": "user_shell", "command": []any{"SECRET"}, "status": "completed"}),
			codexItem(6, "t1", map[string]any{"type": "FileChange", "id": "f1", "changes": map[string]any{"a.go": map[string]any{"diff": "SECRET"}}}),
			codexItem(9, "t1", map[string]any{"type": "CommandExecution", "id": "c3", "source": "agent", "command": []any{"SECRET"}, "status": "completed"}),
			codexItem(11, "t1", map[string]any{"type": "AgentMessage", "id": "a2", "phase": "final_answer", "content": []any{map[string]any{"type": "Text", "text": "All green."}}}),
			rollout(12, map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": "All green.", "error": nil}),
		}
	}
	const finished = "▶ run the tests\n✓ 3 tools (command ×2, edit) · 12s\nAll green."
	rows := []struct {
		name   string
		lines  []string
		events []string
		post   string
	}{{
		name:   "terminal prompt",
		lines:  turn(codexPrompt(0, "run the tests\nand fix them", nil)),
		events: []string{Ended},
		post:   finished,
	}, {
		name:   "Buzz prompt",
		lines:  turn(codexPrompt(0, "run the tests", map[string]any{"client_id": clientID})),
		events: []string{Ended},
		post:   "▶ Buzz\n✓ 3 tools (command ×2, edit) · 12s",
	}, {
		// agent-message-queue-7fz review: reasoning and subagent items after
		// the end edited the post with no new work.
		name: "items after the end that change no post",
		lines: append(turn(codexPrompt(0, "run the tests", nil)),
			codexItem(14, "t1", map[string]any{"type": "Reasoning", "id": "r1", "summary_text": []any{"x"}}),
			codexItem(15, "t1", map[string]any{"type": "SubAgentActivity", "id": "s1", "kind": "started"}),
			codexItem(16, "t1", map[string]any{"type": "UserMessage", "id": "u2", "content": []any{map[string]any{"type": "text", "text": "later"}}})),
		events: []string{Ended},
		post:   finished,
	}, {
		// agent-message-queue-7fz review: a turn that failed before any work
		// made no post.
		name:   "error before any work",
		lines:  []string{start, codexPrompt(0, "hi", nil), rollout(2, map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": nil, "error": map[string]any{"message": "SECRET"}})},
		events: []string{Ended},
		post:   "▶ hi\n✗ error · 2s",
	}}
	src := SourceFor("codex")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var st State
			var kinds []string
			post := ""
			for _, l := range row.lines {
				var evs []Event
				st, evs = src.Fold(st, l, carried)
				for _, ev := range evs {
					kinds = append(kinds, ev.Kind)
					post = Render(ev.Turn)
				}
			}
			if !slices.Equal(kinds, row.events) {
				t.Fatalf("events = %q, want %q", kinds, row.events)
			}
			if post != row.post || strings.Contains(post, "SECRET") {
				t.Fatalf("post =\n%s\nwant\n%s", post, row.post)
			}
		})
	}
}

// The mirror reads the Codex thread's current rollout file: the binding's
// Transcript after a rebind, and the file a thread/revert moved the thread
// to, as Codex picks it.
func TestCodexLocateFollowsTheThread(t *testing.T) {
	const thread = "01a18747-db9a-7e92-9cdf-08f39de60875"
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(tmp, "sessions", "2026", "11", "05")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	file := func(name string) string {
		path := filepath.Join(day, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	orig := file("rollout-2026-11-05T09-00-00-" + thread + ".jsonl")
	other := file("rollout-2026-11-05T09-30-00-01a18747-db9a-7e92-9cdf-08f39de60876.jsonl")
	// agent-message-queue-7fz review: a rebind kept reading the old file.
	if got := codexLocate(binding.Binding{Transcript: other}, orig); got != other {
		t.Fatalf("after a rebind: %q, want the new Transcript %q", got, other)
	}
	// agent-message-queue-7fz review: the file a thread/revert moved the
	// thread to was never read.
	reverted := file("rollout-2026-11-05T09-10-00-" + thread + "_01a18747-0000-7000-8000-000000000001.jsonl")
	file("rollout-2026-11-05T09-10-00-" + thread + "_01a18747-0000-7000-8000-000000000000.jsonl")
	b := binding.Binding{Transcript: orig}
	if got := codexLocate(b, orig); got != reverted {
		t.Fatalf("after a revert: %q, want the newest file %q", got, reverted)
	}
	if err := os.Rename(orig, orig+".zst"); err != nil {
		t.Fatal(err)
	}
	if got := codexLocate(b, orig); got != reverted {
		t.Fatalf("original compressed: %q, want %q", got, reverted)
	}
	// agent-message-queue-7fz review: a binding moved to a thread with no
	// rollout kept reading the old thread's file into the new channel.
	moved := binding.Binding{}
	moved.AppServer.Thread = "01a18747-db9a-7e92-9cdf-08f39de60877"
	if got := codexLocate(moved, reverted); got != "" {
		t.Fatalf("moved to a thread with no rollout: %q, want none", got)
	}

	// agent-message-queue-7fz live check: a plain Codex TUI's binding had no
	// Transcript, so its turns were never read. The rollout is the one this
	// binding's holder (here the test process) keeps open; a guardian's
	// thread open beside it is not, and two user threads are not one.
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	open := func(name, source, threadSource string) string {
		path := filepath.Join(day, name)
		meta := `{"timestamp":"2026-11-05T10:00:00Z","type":"session_meta","payload":{"id":"x","source":` + source + `,"thread_source":"` + threadSource + `","history_mode":"paginated"}}` + "\n"
		if err := os.WriteFile(path, []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return path
	}
	held := binding.Binding{Holder: holder}
	if got := codexLocate(held, ""); got != "" {
		t.Fatalf("holder with no open rollout: %q, want none", got)
	}
	user := open("rollout-2026-11-05T10-00-00-01a18747-db9a-7e92-9cdf-08f39de60880.jsonl", `"cli"`, "user")
	open("rollout-2026-11-05T10-00-01-01a18747-db9a-7e92-9cdf-08f39de60881.jsonl", `{"subagent":{"other":"guardian"}}`, "guardian_review")
	if got := codexLocate(held, ""); got != user {
		t.Fatalf("holder with one open user rollout: %q, want %q", got, user)
	}
	open("rollout-2026-11-05T10-00-02-01a18747-db9a-7e92-9cdf-08f39de60882.jsonl", `"cli"`, "user")
	if got := codexLocate(held, ""); got != "" {
		t.Fatalf("holder with two open user rollouts: %q, want none", got)
	}
}
