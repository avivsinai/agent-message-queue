package mirror

import (
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// piSource reads the activity file the AMQ pi bridge writes. One turn is one
// pi run: the records from its run_start to its settled record. The bridge
// creates the file at its first record, so the file may not exist yet.
var piSource = Source{
	Locate: piLocate,
	Time: func(line string) int64 {
		if n, ok := pi.ParseActivityLine(line); ok {
			return n.At
		}
		return 0
	},
	Fold: func(st State, line string, carried Carried) (State, []Event) {
		n, ok := pi.ParseActivityLine(line)
		if !ok {
			return st, nil
		}
		return foldPi(st, n, carried)
	},
	Lazy: true,
}

// piActivityVersion is the first activity version that records runs.
const piActivityVersion = 2

// piLocate is the activity file of the session the live bridge runs. A live
// bridge that records no runs has none. Without a live bridge it is the file
// the mirror already reads (saved), else, the first time, the file of the
// session the binding was shared for.
func piLocate(b binding.Binding, saved string) string {
	handle := b.Handle
	if !b.Mailbox() {
		handle = strings.TrimPrefix(b.Target, "pi:")
	}
	if _, session, activity, ok := pi.LiveBridge(b.Root, handle, time.Now()); ok {
		switch {
		case activity < piActivityVersion:
			return ""
		case session != "":
			return pi.ActivityFile(b.Root, handle, session)
		}
	}
	if saved != "" {
		return saved
	}
	return pi.ActivityFile(b.Root, handle, b.RecordedSession())
}

// foldPi applies one pi activity record. A record with no run is skipped
// (a bridge that writes no runs). A record of another run than the open
// turn's first ends that turn as unfinished: its settled record was lost.
// A record of a run whose run_start was not seen is skipped, so a turn never
// gets a wrong cause.
func foldPi(st State, n pi.ActivityNote, carried Carried) (State, []Event) {
	switch n.Kind {
	case "run_start", "user", "assistant", "tool_start", "tool_end", "turn_start", "turn_end", "settled":
	default:
		return st, nil
	}
	if n.Run == "" {
		return st, nil
	}
	st = st.clone()
	var out []Event
	if st.Open != nil && st.Open.ID != n.Run {
		out = st.end(EndUnfinished, out)
	}
	t := st.Open
	if n.Kind == "run_start" {
		if t == nil {
			t = &Turn{ID: n.Run, StartTS: n.At}
			if st.Last != nil {
				t.StartTS = max(t.StartTS, st.Last.LastTS)
			}
			st.Open = t
		}
		t.touch(n.At)
		return st, out
	}
	if t == nil {
		return st, out
	}
	t.touch(n.At)
	switch n.Kind {
	case "user":
		if t.Cause != "" {
			// A steer or follow-up inside the run.
			if carried.has(n.ID) {
				t.AnsweredBuzz()
			}
			break
		}
		switch {
		case carried.has(n.ID):
			t.Cause = CauseBuzz
		case strings.HasPrefix(strings.TrimSpace(n.Text), harness.Doorbell):
			t.Cause = CauseMail
		default:
			t.Cause = promptCause(n.Text)
		}
	case "assistant":
		if text := strings.TrimSpace(n.Text); text != "" {
			t.Text = bound(text)
		}
		t.work(n.At)
	case "tool_start":
		t.count(n.Tool)
		t.work(n.At)
	case "settled":
		if t.Cause == "" {
			t.Cause = "continued"
		}
		out = st.end(piEnd(n.Status), out)
	}
	return st, out
}

// work marks a turn as one with an answer, so its end makes a post.
func (t *Turn) work(ts int64) {
	if len(t.MessageIDs) == 0 {
		t.MessageIDs = append(t.MessageIDs, t.ID)
	}
	if t.FirstAssistantTS == 0 {
		t.FirstAssistantTS = ts
	}
}

// piEnd is the end kind of a settled record's status.
func piEnd(status string) string {
	switch status {
	case "completed":
		return EndDone
	case "aborted":
		return EndInterrupted
	case "error":
		return EndError
	}
	return EndUnfinished
}
