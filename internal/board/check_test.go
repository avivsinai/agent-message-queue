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
		// it must not be reported as neglecting a card it still runs. Its
		// sent and read mail show it attends its mailbox, not that it works.
		{name: "idle seat without work evidence stays quiet", state: StationIdle, stage: StageInProgress, card: "one"},
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
				m.Stations[0].WorkActivity = m.Stations[0].LastActivity
			}
			if row.name == "idle seat without work evidence stays quiet" {
				pi := RosterAgent{RosterEntry: RosterEntry{
					Session: "s1", Handle: "amit", ValidHandle: true, Counted: true, Harness: "pi",
					PresenceStatus: "active", PresenceSource: "notifier_live",
					LastSentAt: now.Add(-time.Hour), LastReadAt: now.Add(-time.Hour),
				}}
				st := newStation(pi, true, now, DefaultConfig(), now)
				if st.State != StationIdle {
					t.Fatalf("pi seat state = %q (%s), want idle", st.State, st.StateWhy)
				}
				st.Now = []string{"one"}
				m.Stations[0] = st
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
// reached limit on a mapped worker with a card could report. The freshest
// fresh cap reading at 90 percent or more is itself the floor problem.
func TestCheckReportsAccountNearCap(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.Local)
	codex := usage.Codex + ":~/workspace/.home/codex"
	capAt := func(name, status string, asOf time.Time) usage.Limit {
		return usage.Limit{Kind: "cap", Name: name, Status: status, AsOf: asOf}
	}
	check := func(limits ...usage.Limit) []Problem {
		m := Model{PublishedAt: now.Add(-time.Minute), Usage: &UsageView{Accounts: []AccountUsage{{
			Key: codex, Harness: usage.Codex, Limits: limits,
		}}}}
		problems, err := Check(m, DefaultConfig(), now)
		if err != nil {
			t.Fatal(err)
		}
		return problems
	}
	for _, row := range []struct {
		name   string
		limits []usage.Limit
		want   string
	}{
		{
			name: "a cap 93 percent used is the problem",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-time.Minute))
				l.UsedPercent = ptrFloat(93)
				return l
			}()},
			want: "account Codex ~/workspace/.home/codex: 93% of 100% used (7% left)",
		},
		{
			name: "a cap 80 percent used stays quiet",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-time.Minute))
				l.UsedPercent = ptrFloat(80)
				return l
			}()},
		},
		{
			name: "a stale cap fact stays quiet",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "stale", now.Add(-20*time.Minute))
				l.UsedPercent = ptrFloat(93)
				return l
			}()},
		},
		{
			name: "a newer half-used cap outranks an older 95 percent one",
			limits: []usage.Limit{
				func() usage.Limit {
					l := capAt("spend cap", "fresh", now.Add(-5*time.Minute))
					l.UsedPercent = ptrFloat(95)
					return l
				}(),
				func() usage.Limit {
					l := capAt("spend cap", "fresh", now.Add(-time.Minute))
					l.UsedPercent = ptrFloat(50)
					return l
				}(),
			},
		},
		{
			name: "exact cap figures are short and keep a small remainder visible",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-time.Minute))
				l.Used, l.Cap = "999.468299984932", "1000"
				return l
			}()},
			want: "account Codex ~/workspace/.home/codex: 999.4 of 1000 used (under 0.1% left)",
		},
		{
			name: "exactly one tenth of one percent left is not below one tenth",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-time.Minute))
				l.Used, l.Cap = "999", "1000"
				return l
			}()},
			want: "account Codex ~/workspace/.home/codex: 999 of 1000 used (0.1% left)",
		},
		{
			name: "an old reading inside its reset period is still the cap fact",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-3*time.Hour))
				l.Used, l.Cap = "993", "1000"
				resets := time.Date(2026, 11, 28, 14, 0, 0, 0, time.Local)
				l.ResetsAt = &resets
				return l
			}()},
			want: "account Codex ~/workspace/.home/codex: 993 of 1000 used (0.7% left), resets 14:00",
		},
		{
			name: "a cap without a reset whose own reading is 16 minutes old stays quiet",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-16*time.Minute))
				l.UsedPercent = ptrFloat(93)
				return l
			}()},
		},
		{
			name: "exact cap figures print raw with the share left",
			limits: []usage.Limit{func() usage.Limit {
				l := capAt("spend cap", "fresh", now.Add(-time.Minute))
				l.Used, l.Cap = "993", "1000"
				// resetTime prints the local clock; fix the reset at 14:00
				// local so the expected line holds in every zone.
				resets := time.Date(2026, 12, 1, 14, 0, 0, 0, time.Local)
				l.ResetsAt = &resets
				return l
			}()},
			want: "account Codex ~/workspace/.home/codex: 993 of 1000 used (0.7% left), resets Dec 1 14:00",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			problems := check(row.limits...)
			if row.want == "" {
				if len(problems) != 0 {
					t.Fatalf("false near-cap problem: %+v", problems)
				}
				return
			}
			if len(problems) != 1 || problems[0].Kind != ProblemAccountNearCap || problems[0].Text != row.want {
				t.Fatalf("problems = %+v, want one %s with text %q", problems, ProblemAccountNearCap, row.want)
			}
		})
	}
}

// One notice per account while it is near cap. Neither verbose figures nor
// a one-second provider reset timestamp change its identity; use dropping
// below the threshold clears the delivered record for a later occurrence.
func TestAccountNearCapIncidentKeysOnAccountAndReset(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	resets := now.Add(3 * 24 * time.Hour)
	p := func(reset *time.Time) Problem {
		l := usage.Limit{Kind: "cap", Name: "spend cap", Status: "fresh", Used: "991", Cap: "1000",
			ResetsAt: reset, AsOf: now.Add(-time.Minute)}
		m := Model{PublishedAt: now.Add(-time.Minute), Usage: &UsageView{Accounts: []AccountUsage{{
			Key: usage.Codex + ":~/workspace/.home/codex", Harness: usage.Codex, Limits: []usage.Limit{l},
		}}}}
		problems, err := Check(m, DefaultConfig(), now)
		if err != nil || len(problems) != 1 {
			t.Fatalf("problems = %+v err = %v", problems, err)
		}
		return problems[0]
	}
	first := p(&resets)
	again := p(&resets)
	if again.Incident() != first.Incident() || first.Incident() == "" {
		t.Fatalf("the same period changed incident identity: %q vs %q", again.Incident(), first.Incident())
	}
	next := resets.Add(time.Second)
	// Codex has moved resets_at by one second between readings; that is
	// not a new account condition and must not repeat the notice.
	if other := p(&next).Incident(); other != first.Incident() {
		t.Fatalf("a one-second reset change made another incident: %q vs %q", other, first.Incident())
	}
	otherAccount := p(&resets)
	otherAccount.Account = "codex:~/other-home"
	if otherAccount.Incident() == first.Incident() {
		t.Fatal("two accounts share a near-cap incident identity")
	}
}

func ptrFloat(f float64) *float64 { return &f }
