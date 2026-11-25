package board

import (
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name         string
		state        StationState
		stage        Stage
		want         ProblemKind
		card         string
		full         string
		requestKey   string
		replyOverdue bool
	}{
		{name: "a stalled seat has unread mail", state: StationStalled, want: ProblemSeatStalled},
		{name: "seat at limit owns one card", state: StationLimit, want: ProblemSeatAtLimit, card: "one"},
		{name: "idle seat holds ready work", state: StationIdle, stage: StageReady, want: ProblemReadyNotBegun, card: "one", full: "s1/amit is idle and has ready work that is not started: First card"},
		{name: "idle owner with activity has not moved card in two hours", state: StationIdle, stage: StageInProgress, want: ProblemNotWorked, card: "one"},
		// A pi seat gave a live but absent LastActivity on 2026-11-18;
		// it must not be reported as neglecting a card it still runs.
		{name: "idle seat without activity evidence stays quiet", state: StationIdle, stage: StageInProgress, card: "one"},
		{name: "working owner is clean", state: StationWorking, stage: StageInProgress, card: "one"},
		{name: "idle owner with activity has not moved card in two hours", state: StationIdle, stage: StageInProgress, want: ProblemNotWorked, card: "one"},
		{name: "a seat with ready work and one in progress is not free", state: StationIdle},
		{name: "working owner is clean", state: StationWorking, stage: StageInProgress, card: "one"},
		// Expired asks and timed owner todos do not raise overdue problems.
		{name: "expired agent question and timed owner todo leave the check clear", replyOverdue: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := Card{ID: "one", Title: "First card", Stage: row.stage}
			if row.stage == StageInProgress {
				c.StageSince = now.Add(-2 * time.Hour)
			}
			m := Model{PublishedAt: now.Add(-time.Minute),
				Stations: []Station{{Session: "s1", Handle: "amit", State: row.state, StateWhy: "idle since 10:00", Now: []string{"one"}}},
				Columns:  []ColumnView{{ID: "todo", Cards: []Card{c}}}}
			if row.name == "a seat with ready work and one in progress is not free" {
				m.Columns[0].Cards[0].Stage = StageInProgress
				m.Stations[0].Now = append(m.Stations[0].Now, "two")
				m.Columns[0].Cards = append(m.Columns[0].Cards, Card{ID: "two", Title: "Next", Stage: StageReady})
			}
			if row.stage == StageInProgress && row.want == ProblemNotWorked {
				m.Stations[0].LastActivity = now.Add(-2 * time.Hour)
			}
			if row.replyOverdue {
				opened := now.Add(-RequestWindow - time.Hour)
				ask := requestMessage("ask", "question", "decision", "reviewer", []string{"amit"}, "s1", opened)
				ask.Subject = "a decision"
				ask.ReplyBy = now.Add(-time.Hour).Format(time.RFC3339)
				todo := requestMessage("owner-todo", "todo", "other", "reviewer", []string{reservedHuman}, "s1", now.Add(-2*time.Hour))
				todo.ReplyBy = ask.ReplyBy
				m = requestSnapshot(ask, todo).Project(DefaultConfig(), now, "test")
				if len(m.Asks) != 1 || m.Asks[0].Key != todo.Key || m.Requests.Older[0].Open {
					t.Fatalf("asks = %+v, older = %+v", m.Asks, m.Requests.Older)
				}
				m.PublishedAt = now
			}
			got, err := Check(m, DefaultConfig(), now)
			if err != nil {
				t.Fatal(err)
			}
			if row.want == "" && len(got) == 0 {
				return
			}
			if len(got) != 1 || got[0].Kind != row.want || got[0].CardID != row.card || got[0].Handle != "amit" || got[0].Request != row.requestKey {
				t.Fatalf("problems = %+v, want one %s with card %q and request %q, owned by amit", got, row.want, row.card, row.requestKey)
			}
			if row.full != "" && got[0].Text != row.full {
				t.Fatalf("text = %q; want %q", got[0].Text, row.full)
			}
		})
	}
	_, err := Check(Model{PublishedAt: now.Add(-2 * time.Hour)}, DefaultConfig(), now)
	if err == nil {
		t.Fatal("an old snapshot was accepted instead of being refused")
	}
}

// Fleet audit, 2026-11-25, agent-message-queue-6cxr: a fresh snapshot with a failed source
// reported a clean Floor. Successful partial reads and disabled sources do not.
func TestCheckReportsFailedSources(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name   string
		source SourceView
		want   bool
	}{
		{name: "bd list failed", source: SourceView{Status: StatusDegraded, ReadFailed: true}, want: true},
		{name: "identity unavailable with a coverage note", source: SourceView{Status: StatusDegraded, ReadFailed: true, Partial: true}, want: true},
		{name: "capacity keeps healthy partial facts", source: SourceView{Status: StatusDegraded, Partial: true}},
		{name: "unavailable", source: SourceView{Status: StatusUnavailable}},
		{name: "healthy partial", source: SourceView{Status: StatusOK, Partial: true}},
		{name: "disabled", source: SourceView{Status: StatusDisabled}},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.source.Name = "beads"
			row.source.Detail = "read failed"
			m := Model{PublishedAt: now.Add(-time.Minute), Sources: []SourceView{row.source}}
			problems, err := Check(m, DefaultConfig(), now)
			if err != nil {
				t.Fatal(err)
			}
			if !row.want {
				if len(problems) != 0 {
					t.Fatalf("false source failure: %+v", problems)
				}
				return
			}
			wantIncident := `["source_failed","beads"]`
			if len(problems) != 1 || problems[0].Kind != ProblemSourceFailed || problems[0].Source != "beads" || problems[0].Session != "" || problems[0].Text != "source beads did not read: read failed; the board may miss its cards" || problems[0].Incident() != wantIncident {
				t.Fatalf("problems = %+v incident=%q", problems, problems[0].Incident())
			}
		})
	}
}
