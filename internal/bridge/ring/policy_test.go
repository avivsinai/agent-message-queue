package ring

import (
	"testing"
	"time"
)

func TestRingPolicyStartAndStaleDaily(t *testing.T) {
	start := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)

	if !ShouldRing(start, PeerState{MailPresentAtStart: true}) {
		t.Fatal("mail present at start should ring immediately")
	}
	if ShouldRing(start, PeerState{MailPresentAtStart: true, StartRang: true, LastStored: start}) {
		t.Fatal("fresh stored peer should not use the daily stale ladder")
	}

	stale := PeerState{LastStored: start.Add(-StaleAfter)}
	if !ShouldRing(start, stale) {
		t.Fatal("14-day silent peer should ring")
	}
	stale.LastRing = start
	if ShouldRing(start.Add(23*time.Hour), stale) {
		t.Fatal("stale peer should not ring again before 24h")
	}
	if !ShouldRing(start.Add(DailyRing), stale) {
		t.Fatal("stale peer should ring again after 24h")
	}

	neverStored := PeerState{}
	if !ShouldRing(start, neverStored) {
		t.Fatal("owed mail with no STORED should ring")
	}
}
