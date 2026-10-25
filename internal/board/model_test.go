package board

import (
	"testing"
	"time"
)

// Board state/scope review: installed 0.111.0 called a recent sender live
// after its wake stopped. Only a live notifier establishes a running seat.
func TestRosterSeparatesLiveNotifiersFromRecentPresence(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	result := Result{Status: StatusOK,
		AMQ: &AMQContext{Sessions: []SessionInfo{{Name: "s1"}}},
		Roster: []RosterEntry{
			{Session: "s1", Handle: "a-quiet", ValidHandle: true, PresenceStatus: "active", PresenceAgeSeconds: 7200, UnreadCount: 1},
			{Session: "s1", Handle: "b-recent", ValidHandle: true, PresenceStatus: "active", PresenceSource: "recent_activity", PresenceAgeSeconds: 60},
			{Session: "s1", Handle: "z-live", ValidHandle: true, PresenceSource: "notifier_live"},
			{Session: "s1", Handle: "hidden", ValidHandle: true},
		},
	}
	model := Build(Input{Results: []NamedResult{{Name: "amq", Result: result}}, Config: DefaultConfig(), Now: now})
	roster := model.Roster[0]
	if len(roster.Agents) != 3 || len(roster.Quiet) != 1 || roster.Quiet[0].Handle != "hidden" {
		t.Fatalf("visible roster = %+v, quiet = %+v", roster.Agents, roster.Quiet)
	}
	for i, want := range []struct{ handle, state string }{{"z-live", "live"}, {"b-recent", "recent"}, {"a-quiet", "quiet"}} {
		got := roster.Agents[i]
		if got.Handle != want.handle || got.Liveness != want.state {
			t.Errorf("roster[%d] = %s %s, want %s %s", i, got.Handle, got.Liveness, want.handle, want.state)
		}
	}
}
