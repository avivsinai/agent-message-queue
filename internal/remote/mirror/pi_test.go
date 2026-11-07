package mirror

import (
	"fmt"
	"testing"
)

// A pi run is one turn from its run_start to its settled record: a prompt
// typed in the terminal posts its cause, tools, time and last answer; a run
// the Buzz carrier sent posts the same, with Buzz as its cause.
func TestPiFoldRendersTerminalAndBuzzRuns(t *testing.T) {
	const ref = "amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgayq"
	rec := func(run string, ts int, kind, fields string) string {
		return fmt.Sprintf(`{"protocol":"amq:pi-bridge:v1","seq":1,"at":%q,"session":"pi-session-1","turn":"","run":%q,"kind":%q%s}`, at(ts), run, kind, fields)
	}
	lines := []string{
		rec("0f1e2d3c-r1", 0, "run_start", ""),
		rec("0f1e2d3c-r1", 0, "user", `,"text":"run the tests"`),
		rec("0f1e2d3c-r1", 0, "turn_start", ""),
		rec("0f1e2d3c-r1", 1, "assistant", `,"text":"Checking."`),
		rec("0f1e2d3c-r1", 1, "tool_start", `,"id":"call-1","tool":"bash"`),
		rec("0f1e2d3c-r1", 2, "tool_end", `,"id":"call-1","tool":"bash","text":"ok","status":"completed"`),
		rec("0f1e2d3c-r1", 2, "turn_end", ""),
		rec("0f1e2d3c-r1", 2, "turn_start", ""),
		rec("0f1e2d3c-r1", 3, "assistant", `,"text":"All pass."`),
		rec("0f1e2d3c-r1", 3, "turn_end", ""),
		rec("0f1e2d3c-r1", 3, "settled", `,"status":"completed"`),
		rec("0f1e2d3c-r2", 10, "run_start", ""),
		rec("0f1e2d3c-r2", 10, "user", `,"id":"`+ref+`","text":"what changed?"`),
		rec("0f1e2d3c-r2", 10, "turn_start", ""),
		rec("0f1e2d3c-r2", 12, "assistant", `,"text":"Two files."`),
		rec("0f1e2d3c-r2", 12, "turn_end", ""),
		rec("0f1e2d3c-r2", 12, "settled", `,"status":"completed"`),
	}
	carried := Carried(func(id string) bool { return id == ref })
	src := SourceFor("pi")
	var st State
	var got []string
	for _, l := range lines {
		var evs []Event
		st, evs = src.Fold(st, l, carried)
		for _, ev := range evs {
			if ev.Kind != Ended {
				t.Fatalf("event %s, want only %s", ev.Kind, Ended)
			}
			got = append(got, Render(ev.Turn))
		}
	}
	want := []string{
		"▶ run the tests\n✓ 1 tool (bash) · 3s\nAll pass.",
		"▶ Buzz\n✓ 2s\nTwo files.",
	}
	if len(got) != len(want) {
		t.Fatalf("posts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("post %d = %q, want %q", i, got[i], want[i])
		}
	}
	if st.Open != nil {
		t.Errorf("a turn is still open after the last settle: %+v", st.Open)
	}
}
