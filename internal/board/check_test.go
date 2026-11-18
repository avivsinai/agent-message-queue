package board

import (
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name  string
		state StationState
		stage Stage
		want  ProblemKind
		card  string
		full  string
	}{
		{name: "a stalled seat has unread mail", state: StationStalled, want: ProblemSeatStalled},
		{name: "seat at limit owns one card", state: StationLimit, want: ProblemSeatAtLimit, card: "one"},
		{name: "idle seat holds ready work", state: StationIdle, stage: StageReady, want: ProblemReadyNotBegun, card: "one", full: "s1/amit is idle and has ready work that is not started: First card"},
		{name: "idle owner has not moved card in two hours", state: StationOff, stage: StageInProgress, want: ProblemNotWorked, card: "one"},
		{name: "working owner is clean", state: StationWorking, stage: StageInProgress, card: "one"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := Card{ID: "one", Title: "First card", Stage: row.stage}
			if row.stage == StageInProgress {
				c.StageSince = now.Add(-2 * time.Hour)
			}
			m := Model{PublishedAt: now.Add(-time.Minute),
				Stations: []Station{{Session: "s1", Handle: "amit", State: row.state, StateWhy: "idle since 10:00", Now: []string{"one"}}},
				Columns:  []ColumnView{{ID: "todo", Cards: []Card{c}}}}
			got, err := Check(m, DefaultConfig(), now)
			if err != nil {
				t.Fatal(err)
			}
			if row.want == "" && len(got) == 0 {
				return
			}
			if len(got) != 1 || got[0].Kind != row.want || got[0].CardID != row.card {
				t.Fatalf("problems = %+v, want one %s with card %q", got, row.want, row.card)
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
