package ring

import "time"

const (
	StaleAfter = 14 * 24 * time.Hour
	DailyRing  = 24 * time.Hour
)

// PeerState is the durable facts the start and stale-daily policy reads.
type PeerState struct {
	MailPresentAtStart bool
	StartRang          bool
	LastStored         time.Time
	LastRing           time.Time
}

// ShouldRing reports whether up should fire a content-free ring now.
// The normal 5s-to-2m wake ladder stays in amq wake.
func ShouldRing(now time.Time, state PeerState) bool {
	if state.MailPresentAtStart && !state.StartRang {
		return true
	}
	if !stalePeer(now, state.LastStored) {
		return false
	}
	if state.LastRing.IsZero() {
		return true
	}
	return !now.Before(state.LastRing.Add(DailyRing))
}

func stalePeer(now, lastStored time.Time) bool {
	if lastStored.IsZero() {
		return true
	}
	return !now.Before(lastStored.Add(StaleAfter))
}
