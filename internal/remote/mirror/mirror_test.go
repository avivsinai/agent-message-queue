package mirror

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// base is the time of the first synthetic line; at(n) is n seconds later.
var base = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

func at(n int) string { return base.Add(time.Duration(n) * time.Second).Format(time.RFC3339Nano) }

// line is one synthetic transcript line: fields set at the top level, with
// content as message.content.
func line(fields map[string]any, content ...map[string]any) string {
	if content != nil {
		msg, _ := fields["message"].(map[string]any)
		if msg == nil {
			msg = map[string]any{}
		}
		msg["content"] = content
		fields["message"] = msg
	}
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func prompt(uuid, promptID string, ts int, text string) string {
	return line(map[string]any{"type": "user", "uuid": uuid, "promptId": promptID, "timestamp": at(ts), "origin": map[string]any{"kind": "human"}},
		map[string]any{"type": "text", "text": text})
}

func user(promptID string, ts int, extra map[string]any, content ...map[string]any) string {
	f := map[string]any{"type": "user", "uuid": "u-" + at(ts), "promptId": promptID, "timestamp": at(ts)}
	for k, v := range extra {
		f[k] = v
	}
	return line(f, content...)
}

func assistant(id, stop string, ts int, extra map[string]any, content ...map[string]any) string {
	f := map[string]any{"type": "assistant", "uuid": "a-" + at(ts), "timestamp": at(ts), "message": map[string]any{"id": id, "stop_reason": stop}}
	for k, v := range extra {
		f[k] = v
	}
	return line(f, content...)
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func thinking() map[string]any     { return map[string]any{"type": "thinking", "thinking": "…"} }
func tool(name string) map[string]any {
	return map[string]any{"type": "tool_use", "id": "t-" + name, "name": name, "input": map[string]any{}}
}
func result(id string, failed bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": "x", "is_error": failed}
}

// Each row is one transcript shape measured in real sessions, written by
// hand. Fold must report the turn events in order, and the latest post of
// each turn (the value of its last event), in the order the turns first
// reported, must match.
func TestFoldRendersEachMeasuredShape(t *testing.T) {
	const carried = "6f1d2c3b-0000-5000-8000-000000000001"
	envelope := "<cross-session-message from=\"amq-peer\">\nhello\n</cross-session-message>"
	rows := []struct {
		name   string
		lines  []string
		events []string
		posts  []string
	}{{
		name: "closing message with a thinking line, then a text line",
		lines: []string{
			prompt("o1", "p1", 2, "run the tests and fix the failure\nthen commit"),
			assistant("m1", "tool_use", 2, nil, tool("Bash")),
			user("p1", 3, nil, result("t-Bash", false)),
			assistant("m2", "tool_use", 4, nil, tool("Read"), tool("Bash")),
			user("p1", 5, nil, result("t-Read", false)),
			assistant("m3", "end_turn", 70, nil, thinking()),
			assistant("m3", "end_turn", 74, nil, text("Fixed the off-by-one; all tests pass.")),
		},
		events: []string{Opened, Progress, Progress, Ended},
		posts:  []string{"▶ run the tests and fix the failure\n✓ 3 tools (Bash ×2, Read) · 1m12s\nFixed the off-by-one; all tests pass."},
	}, {
		name: "a peer line between the two lines of the closing message",
		lines: []string{
			prompt("o1", "p1", 0, "summarize"),
			assistant("m1", "end_turn", 1, nil, thinking()),
			user("p2", 2, map[string]any{"origin": map[string]any{"kind": "peer", "msg_id": "peer-1"}}, text(envelope)),
			assistant("m1", "end_turn", 3, nil, text("Summary.")),
		},
		events: []string{Opened, Ended, Continued},
		posts:  []string{"▶ summarize\n✓ 3s\nSummary."},
	}, {
		name: "a shell command typed during a turn",
		lines: []string{
			prompt("o1", "p1", 0, "check the build"),
			assistant("m1", "tool_use", 1, nil, tool("Read")),
			user("p9", 2, nil, text("<bash-input>ls</bash-input>")),
			user("p9", 3, nil, text("<bash-stdout>a b</bash-stdout><bash-stderr></bash-stderr>")),
			user("p1", 4, nil, result("t-Read", false)),
			assistant("m2", "end_turn", 6, nil, text("Build is clean.")),
		},
		events: []string{Opened, Ended},
		posts:  []string{"▶ check the build\n✓ 1 tool (Read) · 6s\nBuild is clean."},
	}, {
		name: "a local slash command",
		lines: []string{
			prompt("o1", "p1", 0, "<command-name>/reload-plugins</command-name>\n<command-message>reload-plugins</command-message>"),
			user("p1", 1, nil, text("<local-command-stdout>Reloaded 3 plugins</local-command-stdout>")),
			prompt("o2", "p2", 2, "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>42</command-args>"),
			assistant("m1", "end_turn", 3, nil, text("Looks good.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ /review\n✓ 1s\nLooks good."},
	}, {
		name: "a compaction summary inside a turn",
		lines: []string{
			prompt("o1", "p1", 0, "refactor the parser"),
			assistant("m1", "tool_use", 1, nil, tool("Edit")),
			user("p1", 2, nil, result("t-Edit", false)),
			user("p2", 3, map[string]any{"isCompactSummary": true, "isVisibleInTranscriptOnly": true}, text("This session is being continued from a previous conversation.")),
			assistant("m2", "end_turn", 9, nil, text("Refactored.")),
		},
		events: []string{Opened, Ended},
		posts:  []string{"▶ refactor the parser\n✓ 1 tool (Edit) · 9s\nRefactored."},
	}, {
		name: "a Stop hook that refused the stop",
		lines: []string{
			prompt("o1", "p1", 0, "ship it"),
			assistant("m1", "end_turn", 1, nil, text("Shipped.")),
			user("p1", 2, map[string]any{"isMeta": true}, text("Stop hook feedback: tests were not run")),
			assistant("m2", "tool_use", 3, nil, tool("Bash")),
			user("p1", 4, nil, result("t-Bash", false)),
			assistant("m3", "end_turn", 5, nil, text("Tests pass; shipped.")),
		},
		events: []string{Ended, Progress, Continued},
		posts:  []string{"▶ ship it\n✓ 1 tool (Bash) · 5s\nTests pass; shipped."},
	}, {
		name: "a deny with feedback, then the interrupt text and an answer",
		lines: []string{
			prompt("o1", "p1", 0, "clean the tree"),
			assistant("m1", "tool_use", 1, nil, tool("Bash")),
			user("p1", 2, nil, result("t-Bash", true)),
			user("p1", 2, nil, text("use git clean -n first")),
			user("p1", 2, nil, text("[Request interrupted by user for tool use]")),
			assistant("m2", "end_turn", 3, nil, text("Running a dry run instead.")),
		},
		events: []string{Opened, Progress, Ended},
		posts:  []string{"▶ clean the tree\n✓ 1 tool (Bash) · 3s\nRunning a dry run instead."},
	}, {
		// agent-message-queue-07h review: an interrupted turn kept showing
		// "running" while the session sat idle.
		name: "an interrupt with no answer after it",
		lines: []string{
			prompt("o1", "p1", 0, "run the suite"),
			assistant("m1", "tool_use", 1, nil, tool("Bash")),
			user("p1", 2, nil, text("[Request interrupted by user]")),
		},
		events: []string{Opened, Progress},
		posts:  []string{"▶ run the suite\n✗ interrupted · 1 tool (Bash) · 2s"},
	}, {
		name: "an auto-continuation",
		lines: []string{
			prompt("o1", "p1", 0, "write the report"),
			assistant("m1", "end_turn", 1, nil, text("Part one.")),
			user("p2", 2, map[string]any{"isMeta": true, "origin": map[string]any{"kind": "auto-continuation"}}, text("Continue from where you left off.")),
			assistant("m2", "end_turn", 4, nil, text("Part two.")),
		},
		events: []string{Ended, Ended},
		posts:  []string{"▶ write the report\n✓ 1s\nPart one.", "▶ continued\n✓ 2s\nPart two."},
	}, {
		name: "a scheduled fire",
		lines: []string{
			user("p1", 0, map[string]any{"isMeta": true, "turnOrigin": "scheduled", "scheduledTaskId": "job-1"}, text("check the deploy")),
			assistant("m1", "end_turn", 2, nil, text("Deploy is healthy.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ scheduled task\n✓ 2s\nDeploy is healthy."},
	}, {
		name: "a shell command the model answers",
		lines: []string{
			user("p1", 0, nil, text("<bash-input>make test</bash-input>")),
			user("p1", 1, nil, text("<bash-stdout>FAIL</bash-stdout><bash-stderr></bash-stderr>")),
			assistant("m1", "end_turn", 3, nil, text("The test fails on a nil map.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ shell command\n✓ 3s\nThe test fails on a nil map."},
	}, {
		name: "a prompt that starts with harness markup",
		lines: []string{
			prompt("o1", "p1", 0, "<system-reminder>\nToday is Tuesday.\n</system-reminder>\nfix the build\nplease"),
			assistant("m1", "end_turn", 1, nil, text("Fixed.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ fix the build\n✓ 1s\nFixed."},
	}, {
		// agent-message-queue-6r9 live check: a doorbell turn posted the doorbell text itself.
		name: "a turn the AMQ doorbell started",
		lines: []string{
			prompt("o1", "p1", 0, harness.Doorbell),
			assistant("m1", "end_turn", 2, nil, text("Drained one message.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ AMQ message\n✓ 2s\nDrained one message."},
	}, {
		// agent-message-queue-6r9 review: a native wake sends the doorbell
		// as a peer frame, which posted as a message from another session.
		name: "a doorbell a native wake sent as a peer frame",
		lines: []string{
			user("p1", 0, map[string]any{"uuid": "o1", "isMeta": true, "origin": map[string]any{"kind": "peer", "msg_id": "0d0b0e11-0000-4000-8000-000000000001"}},
				text("<cross-session-message from=\"amq-doorbell\" from-session=\"amq\" from-name=\"amq-wake\" from-mode=\"code\">\n"+harness.Doorbell+"\n</cross-session-message>")),
			assistant("m1", "end_turn", 2, nil, text("Drained one message.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ AMQ message\n✓ 2s\nDrained one message."},
	}, {
		// agent-message-queue-07h design review: Buzz refuses a post with an
		// "@name" that matches no channel member, so the turn was lost.
		name: "a name after @ in the prompt and the answer",
		lines: []string{
			prompt("o1", "p1", 0, "@amit check user@host"),
			assistant("m1", "end_turn", 1, nil, text("Asked @amit.")),
		},
		events: []string{Ended},
		posts:  []string{"▶ @\u2060amit check user@host\n✓ 1s\nAsked @\u2060amit."},
	}, {
		name: "sidechain lines of a subagent",
		lines: []string{
			prompt("o1", "p1", 0, "find the bug"),
			assistant("m1", "tool_use", 1, nil, tool("Task")),
			user("s1", 2, map[string]any{"isSidechain": true}, text("search for nil maps")),
			assistant("sm1", "end_turn", 3, map[string]any{"isSidechain": true}, tool("Grep"), text("found it")),
			user("p1", 4, nil, result("t-Task", false)),
			assistant("m2", "end_turn", 5, nil, text("The bug is in cache.go.")),
		},
		events: []string{Opened, Ended},
		posts:  []string{"▶ find the bug\n✓ 1 tool (Task) · 5s\nThe bug is in cache.go."},
	}, {
		name: "a Buzz frame absorbed into the running turn",
		lines: []string{
			prompt("o1", "p1", 0, "deploy"),
			assistant("m1", "tool_use", 1, nil, tool("Bash")),
			line(map[string]any{"type": "attachment", "uuid": "q1", "timestamp": at(2),
				"attachment": map[string]any{"type": "queued_command", "prompt": envelope, "origin": map[string]any{"kind": "peer", "msg_id": carried}}}),
			user("p1", 3, nil, result("t-Bash", false)),
			assistant("m2", "end_turn", 4, nil, text("Deployed.")),
		},
		events: []string{Opened, Ended},
		posts:  []string{"▶ deploy\n✓ 1 tool (Bash) · 4s"},
	}, {
		name: "an API error end",
		lines: []string{
			prompt("o1", "p1", 0, "hello"),
			assistant("e1", "stop_sequence", 1, map[string]any{"isApiErrorMessage": true}, text("API Error: 529 overloaded")),
		},
		events: []string{Ended},
		posts:  []string{"▶ hello\n✗ error · 1s\nAPI Error: 529 overloaded"},
	}}
	isCarried := func(id string) bool { return id == carried }
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var st State
			var kinds, order []string
			posts := map[string]string{}
			for _, raw := range row.lines {
				l, ok := claude.ParseTranscriptLine(raw)
				if !ok {
					t.Fatalf("line does not parse: %s", raw)
				}
				var evs []Event
				st, evs = Fold(st, l, isCarried)
				for _, ev := range evs {
					kinds = append(kinds, ev.Kind)
					if _, seen := posts[ev.Turn.ID]; !seen {
						order = append(order, ev.Turn.ID)
					}
					posts[ev.Turn.ID] = Render(ev.Turn)
				}
			}
			if !slices.Equal(kinds, row.events) {
				t.Fatalf("events = %q; want %q", kinds, row.events)
			}
			var got []string
			for _, id := range order {
				got = append(got, posts[id])
			}
			if !slices.Equal(got, row.posts) {
				t.Fatalf("posts =\n%q\nwant\n%q", got, row.posts)
			}
		})
	}
}
