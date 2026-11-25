package board

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
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

// account_near_cap, from the 2026-11-25 tear-down: the Codex account went
// from 761 to 993 of a 1,000-unit cap with no floor problem, because only a
// reached limit on a mapped worker with a card could report. A fresh cap or
// window reading at 90 percent or more is itself the floor problem.
func TestCheckReportsAccountNearCap(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	pct := func(p float64) *float64 { return &p }
	for _, row := range []struct {
		name       string
		kind       string
		status     string
		used, left *float64
		want       bool
	}{
		{name: "a cap 93 percent used is the problem", kind: "cap", status: "fresh", used: pct(93), want: true},
		{name: "a window 91 percent used is the problem", kind: "window", status: "fresh", used: pct(91), want: true},
		{name: "exactly 90 percent used is the problem", kind: "cap", status: "fresh", used: pct(90), want: true},
		{name: "a cap 80 percent used stays quiet", kind: "cap", status: "fresh", used: pct(80)},
		{name: "a stale cap fact stays quiet", kind: "cap", status: "stale", used: pct(93)},
		{name: "a reading with no status stays quiet", kind: "cap", used: pct(93)},
		{name: "a limit kind that is neither cap nor window stays quiet", kind: "credits", status: "fresh", used: pct(93)},
	} {
		t.Run(row.name, func(t *testing.T) {
			l := usage.Limit{Kind: row.kind, Name: "spend cap", Status: row.status, AsOf: now.Add(-time.Minute)}
			if row.used != nil {
				l.UsedPercent = row.used
			}
			if row.left != nil {
				l.RemainingPercent = row.left
			}
			m := Model{PublishedAt: now.Add(-time.Minute), Usage: &UsageView{Accounts: []AccountUsage{{
				Key: usage.Codex + ":~/workspace/.home/codex", Harness: usage.Codex, Limits: []usage.Limit{l},
			}}}}
			problems, err := Check(m, DefaultConfig(), now)
			if err != nil {
				t.Fatal(err)
			}
			if !row.want {
				if len(problems) != 0 {
					t.Fatalf("false near-cap problem: %+v", problems)
				}
				return
			}
			if len(problems) != 1 || problems[0].Kind != ProblemAccountNearCap {
				t.Fatalf("problems = %+v, want one %s", problems, ProblemAccountNearCap)
			}
		})
	}
}

// One notice per account per reset period: the incident identity carries the
// account and its reset, not the prose or the numbers, so 991 of 1,000 and
// 993 of 1,000 are one incident and a new reset period sends again.
func TestAccountNearCapIncidentKeysOnAccountAndReset(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	resets := now.Add(3 * 24 * time.Hour)
	p := func(used string, reset *time.Time) Problem {
		l := usage.Limit{Kind: "cap", Name: "spend cap", Status: "fresh", Used: used, Cap: "1000",
			RemainingPercent: ptrFloat(7), ResetsAt: reset, AsOf: now.Add(-time.Minute)}
		m := Model{PublishedAt: now.Add(-time.Minute), Usage: &UsageView{Accounts: []AccountUsage{{
			Key: usage.Codex + ":~/workspace/.home/codex", Harness: usage.Codex, Limits: []usage.Limit{l},
		}}}}
		problems, err := Check(m, DefaultConfig(), now)
		if err != nil || len(problems) != 1 {
			t.Fatalf("problems = %+v err = %v", problems, err)
		}
		return problems[0]
	}
	first := p("991", &resets)
	if first.Text != "account Codex ~/workspace/.home/codex: 991.00 of 1000.00 used (7% left), resets Dec 1 14:00" {
		t.Fatalf("text = %q", first.Text)
	}
	if first.Incident() != first.Incident() {
		t.Fatal("incident identity is not stable")
	}
	again := p("993", &resets)
	if again.Incident() != first.Incident() {
		t.Fatalf("a new reading of the same period re-notified: %q vs %q", again.Incident(), first.Incident())
	}
	next := resets.Add(7 * 24 * time.Hour)
	if p("991", &next).Incident() == first.Incident() {
		t.Fatal("a new reset period kept the old incident identity")
	}
	noReset := p("991", nil)
	if noReset.Incident() == first.Incident() {
		t.Fatal("a reading with no reset kept the old incident identity")
	}
}

func ptrFloat(f float64) *float64 { return &f }
