package usage

import "time"

// Status is a derived answer about one home's plan limits at one time.
type Status string

const (
	// StatusFresh: windows read inside the stale bound, all reported.
	StatusFresh Status = "fresh"
	// StatusStale: the newest reading is older than the stale bound, or
	// dated after the time asked about.
	StatusStale Status = "stale"
	// StatusPartial: a window slot was null or invalid, or a later attempt
	// failed.
	StatusPartial Status = "partial"
	// StatusUnknown: no window or money fact for the home.
	StatusUnknown Status = "unknown"
)

// Assessment is Assess's answer.
type Assessment struct {
	Status Status
	// ObservedAt is the newest observation of the home's windows and money.
	ObservedAt time.Time
	// Failure is the newest failure, when it is newer than ObservedAt.
	Failure *Failure
}

// Assess derives the status of one provider home's plan limits at now. It
// reads only r and orders observations by their times, never by position.
func Assess(r Result, provider, home string, now time.Time, staleAfter time.Duration) Assessment {
	f := r.ForHome(provider, home)
	var a Assessment
	partial := false
	for _, w := range f.Windows {
		a.ObservedAt = later(a.ObservedAt, w.Evidence.ObservedAt)
		v := w.Value
		if v.Reported != Present || v.UsedPercent.Presence == Invalid || v.ResetsAt.Presence == Invalid || v.DurationMinutes.Presence == Invalid {
			partial = true
		}
	}
	for _, m := range f.Money {
		a.ObservedAt = later(a.ObservedAt, m.Evidence.ObservedAt)
		if m.Value.Amount.Presence == Invalid || m.Value.Limit.Presence == Invalid {
			partial = true
		}
	}
	var failedAt time.Time
	for _, x := range f.Failures {
		if t := x.Evidence.ObservedAt; a.Failure == nil || t.After(failedAt) {
			v := x.Value
			a.Failure, failedAt = &v, t
		}
	}
	if a.Failure != nil && !a.ObservedAt.IsZero() && !failedAt.After(a.ObservedAt) {
		a.Failure = nil
	}
	switch {
	case len(f.Windows) == 0 && len(f.Money) == 0:
		a.Status = StatusUnknown
	case a.ObservedAt.After(now) || now.Sub(a.ObservedAt) > staleAfter:
		a.Status = StatusStale
	case partial || a.Failure != nil:
		a.Status = StatusPartial
	default:
		a.Status = StatusFresh
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
