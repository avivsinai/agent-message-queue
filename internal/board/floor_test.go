package board

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
	"github.com/avivsinai/agent-message-queue/internal/harness"
)

// Each seat gets the first state its evidence supports; an abandoned
// mailbox that receives mail folds into the dormant count.
func TestStationStates(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	resets := now.Add(48 * time.Hour)
	limit := &usage.Limit{Kind: "reached", Name: "seven_day", ResetsAt: &resets, AsOf: now.Add(-2 * time.Hour)}
	seat := func(presenceAge time.Duration, unread int, oldest time.Duration) RosterAgent {
		return RosterAgent{RosterEntry: RosterEntry{
			Session: "s1", Handle: "worker", ValidHandle: true, Counted: true, PresenceStatus: "active",
			PresenceAgeSeconds: presenceAge.Seconds(), UnreadCount: unread, OldestUnreadAgeSeconds: oldest.Seconds(),
		}}
	}
	live := func(a RosterAgent) RosterAgent { a.PresenceSource = "notifier_live"; return a }

	working := live(seat(5*time.Minute, 0, 0))
	working.LimitReached = limit
	working.Usage = []Usage{{Window: usage.Window5h, AsOf: now.Add(-5 * time.Minute)}}
	limited := live(seat(3*time.Hour, 0, 0))
	limited.LimitReached = limit
	accountActive := live(seat(2*time.Hour, 0, 0))
	accountActive.LimitReached, accountActive.AccountActiveAt = limit, now.Add(-time.Hour)
	// AMQ read 20 min before the render: mail 40 min old then, usage 30
	// min before the read, so the seat worked after the mail arrived.
	lateRender := live(seat(0, 1, 40*time.Minute))
	lateRender.Usage = []Usage{{Window: usage.Window5h, AsOf: now.Add(-50 * time.Minute)}}
	uncounted := live(seat(0, 0, 0))
	uncounted.Counted, uncounted.CountError, uncounted.PresenceStatus = false, "permission denied", "unknown"
	sentBeforeMail := live(seat(time.Minute, 27, 8*time.Hour))
	sentBeforeMail.LastSentAt = now.Add(-9 * time.Hour)
	sentAfterMail := live(seat(time.Minute, 27, 8*time.Hour))
	sentAfterMail.LastSentAt = now.Add(-time.Hour)

	// A measured seat with nothing inside the liveness window reads Quiet
	// with a live wake, and Off without one.
	quietMeasured := live(seat(time.Hour, 0, 0))
	quietMeasured.Usage = []Usage{{Window: usage.Window5h, AsOf: now.Add(-2 * time.Hour)}}
	quietNoWake := seat(time.Hour, 0, 0)
	quietNoWake.Usage = []Usage{{Window: usage.Window5h, AsOf: now.Add(-2 * time.Hour)}}
	for _, tc := range []struct {
		name  string
		seat  RosterAgent
		owns  bool
		state StationState
		// read is how long before the render AMQ was read.
		read time.Duration
	}{
		{"working after its limit was read", working, false, StationWorking, 0},
		{"quiet; its account was active after the limit was read", accountActive, false, StationNotMeasured, 0},
		// Observed in the 2026-11-01 live render: live33c/claude, a wake with
		// no agent work, read Working because the wake refreshes presence.
		// A seat with no usage source is not measured, however fresh its presence.
		{"not measured with fresh wake presence", live(seat(time.Minute, 0, 0)), false, StationNotMeasured, 0},
		// Review of the Floor build: unread ages date from the AMQ read, so
		// a later render must not turn this seat Stalled.
		{"quiet when rendered after the read", lateRender, false, StationQuiet, 20 * time.Minute},
		{"stalled", seat(9*time.Hour, 3, 5*time.Hour), false, StationStalled, 0},
		// Field, 2026-11-26: a pi seat with a live wake, 27 unread for 8 hours
		// and no usage record read Unknown, and check reported nothing.
		{"stalled from its own mail activity", sentBeforeMail, false, StationStalled, 0},
		{"not measured when it sent after the mail arrived; the unread still shows", sentAfterMail, false, StationNotMeasured, 0},
		{"limit", limited, false, StationLimit, 0},
		// A live wake holds the seat out of dormant; with no wake, presence is
		// the one activity fact. Neither proves work: work reads not measured
		// whenever no usage source covers the seat.
		{"off; presence alone is never work", seat(72*time.Hour, 0, 0), true, StationNotMeasured, 0},
		// Presence is the only activity fact on this seat, and no wake is live:
		// it proves attendance, not work, so the seat is not measured.

		{"unknown", uncounted, false, StationUnknown, 0},
		{"quiet, measured, wake live", quietMeasured, false, StationQuiet, 0},
		{"off, measured, no wake", quietNoWake, true, StationOff, 0},
		{"dormant", seat(40*24*time.Hour, 2, 3*time.Hour), false, stationDormant, 0},
	} {
		st := newStation(tc.seat, tc.owns, now.Add(-tc.read), cfg, now)
		if st.State != tc.state {
			t.Errorf("%s: state %q (%s), want %q", tc.name, st.State, st.StateWhy, tc.state)
		}
		if tc.state == StationStalled {
			problems, err := Check(Model{PublishedAt: now, Stations: []Station{st}}, cfg, now)
			if err != nil || len(problems) != 1 || problems[0].Kind != ProblemSeatStalled {
				t.Errorf("%s: check = %+v, %v; want one %s", tc.name, problems, err, ProblemSeatStalled)
			}
		}
	}
	// An unknown harness prints no label, not "other".
	if s := summarizeStation(Station{Harness: harness.Other}, nil, DefaultConfig(), now); s.Harness != "" {
		t.Errorf("unknown harness label = %q, want none", s.Harness)
	}
	if st := newStation(working, false, now, cfg, now); st.LimitNote == "" || st.Limit == nil {
		t.Errorf("a seat active after its limit was read keeps the limit as a note: %+v", st)
	}
}

// Observed 2026-11-01: the account claude:~/.claude reported its 7-day limit
// reached with worker session1/claude, and the published roster showed no
// limit on that worker, because the projection restored the usage view but
// not its per-worker facts.
func TestProjectAttachesAccountLimitToWorker(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	resets := now.Add(48 * time.Hour)
	s := Snapshot{Tasks: map[string]Task{}, Facts: map[string]AliasFact{}, Messages: map[string]Message{}}
	s.Input.Results = []NamedResult{{Name: "amq", Result: Result{
		Status: StatusOK, ObservedAt: now,
		AMQ:    &AMQContext{Sessions: []SessionInfo{{Name: "session1", Path: "/q/session1"}}},
		Roster: []RosterEntry{{Session: "session1", Handle: "claude", ValidHandle: true, Counted: true, PresenceSource: "notifier_live"}},
	}}}
	s.Usage = &UsageView{Accounts: []AccountUsage{{
		Key: "claude:~/.claude", Harness: usage.Claude,
		Limits:  []usage.Limit{{Kind: "reached", Name: "seven_day", ResetsAt: &resets, AsOf: now.Add(-48 * time.Hour)}},
		Workers: []WorkerUsage{{Session: "session1", Handle: "claude", Usage: []Usage{{Window: usage.Window5h, Fresh: 10, AsOf: now}}}},
	}}}

	m := s.Project(DefaultConfig(), now, "test")
	if len(m.Roster) != 1 || len(m.Roster[0].Agents) != 1 {
		t.Fatalf("roster = %+v, want session1/claude", m.Roster)
	}
	a := m.Roster[0].Agents[0]
	if a.LimitReached == nil || a.LimitReached.Name != "seven_day" || len(a.Usage) != 1 {
		t.Errorf("session1/claude has limit %+v and usage %+v, want the account's seven_day limit and its spend", a.LimitReached, a.Usage)
	}
}

// Pre-merge review: a seat on two limited accounts read Idle, because the
// limit of one account was rebutted by the spend of the other.
func TestAccountLimitIsRebuttedOnlyByItsOwnAccount(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	resets := now.Add(48 * time.Hour)
	s := Snapshot{Tasks: map[string]Task{}, Facts: map[string]AliasFact{}, Messages: map[string]Message{}}
	s.Input.Results = []NamedResult{{Name: "amq", Result: Result{
		Status: StatusOK, ObservedAt: now,
		AMQ:    &AMQContext{Sessions: []SessionInfo{{Name: "session1", Path: "/q/session1"}}},
		Roster: []RosterEntry{{Session: "session1", Handle: "claude", ValidHandle: true, Counted: true, PresenceSource: "notifier_live"}},
	}}}
	seat := []WorkerUsage{{Session: "session1", Handle: "claude", Usage: []Usage{{Window: usage.Window5h, Fresh: 10, AsOf: now.Add(-4 * time.Hour)}}}}
	s.Usage = &UsageView{Accounts: []AccountUsage{{
		Key: "claude:~/.claude", Harness: usage.Claude,
		Limits:  []usage.Limit{{Kind: "reached", Name: "five_hour", ResetsAt: &resets, AsOf: now.Add(-5 * time.Hour)}},
		Other:   []Usage{{Window: usage.Window5h, Fresh: 10, AsOf: now.Add(-time.Hour)}},
		Workers: seat,
	}, {
		Key: "codex:~/.codex", Harness: usage.Codex,
		Limits:  []usage.Limit{{Kind: "reached", Name: "seven_day", ResetsAt: &resets, AsOf: now.Add(-3 * time.Hour)}},
		Workers: seat,
	}}}

	a := s.Project(DefaultConfig(), now, "test").Roster[0].Agents[0]
	if a.LimitReached == nil || a.LimitReached.Name != "seven_day" || a.AccountActiveAt.After(a.LimitReached.AsOf) {
		t.Errorf("limit %+v, account active at %v; want the codex seven_day limit with no activity after it", a.LimitReached, a.AccountActiveAt)
	}
}

// Field, 2026-11-25: the board showed a 40-hour-old snapshot as current; a fresh wake does not make the note fresh.
func TestStationNoteUsesItsOwnTimestamp(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct{ name, at, want string }{
		{"old note", now.Add(-2 * time.Hour).Format(time.RFC3339), ""},
		{"young note", now.Add(-5 * time.Minute).Format(time.RFC3339), "waiting (5m ago)"},
		{"legacy record", "", "waiting (age unknown)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			a := RosterAgent{RosterEntry: RosterEntry{Session: "s1", Handle: "alpha", ValidHandle: true, Counted: true, PresenceSource: "notifier_live", PresenceStatus: "active", Note: "waiting", NoteAt: row.at}}
			if got := newStation(a, false, now.Add(-time.Hour), DefaultConfig(), now).Says; got != row.want {
				t.Fatalf("note=%q want %q", got, row.want)
			}
		})
	}
}
