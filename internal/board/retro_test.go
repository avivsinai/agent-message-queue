package board

import (
	"testing"
	"time"
)

func TestRetro(t *testing.T) {
	t0 := requestNow
	todo := requestMessage("todo", "todo", "work", "alice", []string{"bob"}, "s1", t0)
	status := requestMessage("status", "status", "work", "bob", []string{"alice"}, "s1", t0.Add(5*time.Minute), "todo")
	correction := requestMessage("correction", "status", "work", "alice", []string{"bob"}, "s1", t0.Add(10*time.Minute), "todo", "status")
	notice := requestMessage("notice", "status", "work", "alice", []string{"alice"}, "s1", t0.Add(20*time.Minute), "todo")
	notice.Labels = []string{"follow-up"}
	done := requestMessage("done", "status", "work", "bob", []string{"alice"}, "s1", t0.Add(30*time.Minute), "todo", "status", "correction")
	done.Labels = []string{"done"}
	view := requestSnapshot(todo, status, correction, notice, done).Retro(t0.Add(-time.Hour), t0.Add(time.Hour))
	want := RetroSeat{Session: "s1", Handle: "bob", Requests: 1, Replied: 1, Closed: 1, FirstReplyMedian: 300, FirstReplyMax: 300, DoneMedian: 1800, DoneMax: 1800, FollowUps: 1, Rounds: 1, RoundsMax: 1}
	if view.Requests != 1 || len(view.Seats) != 1 || view.Seats[0] != want {
		t.Fatalf("Retro() = %+v, want one request and seat %+v", view, want)
	}
}
