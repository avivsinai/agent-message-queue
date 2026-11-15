package mirror

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
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
		"▶ Buzz\n✓ 2s",
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

// S5: a native pi binding carries a Seat, and its activity path comes from
// the session's own root, not the driver's root. Without the seat it would
// derive the wrong handle from a place-target's Target.
func TestPiLocateUsesSeatOnAPlaceNamedTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := dir + "/queue"
	handle := "agent1"
	bridge := root + "/agents/" + handle + "/extensions/pi-bridge"
	const wantRevision = 4
	at := time.Now().UTC().Format(time.RFC3339Nano)
	record := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"rpc","session_id":"sess-1","bridge_revision":%d,"activity":3}`, "amq:pi-bridge:v1", at, os.Getpid(), wantRevision)
	if err := os.MkdirAll(bridge, 0o700); err != nil {
		t.Fatal(err)
	}
	path := bridge + "/bridge.liveness"
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	var b binding.Binding
	b.Root = "/some/other/root"
	b.Seat = binding.Seat{Root: root, Handle: handle}
	b.Target = "pi:agent-message-queue-s1-amit"
	b.NativeSession = "sess-1"
	got := piLocate(b, "")
	if got != pi.ActivityFile(root, handle, "sess-1") {
		t.Fatalf("piLocate used the driver's root: %q; want %q", got, pi.ActivityFile(root, handle, "sess-1"))
	}
}
