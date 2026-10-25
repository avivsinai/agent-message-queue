// Package usagehold is the pure pacing of usage collection: which reserved
// attempts conflict, how long an attempt holds the next one back, which home
// goes next, and which attempts the journal keeps. Times are values; nothing
// here reads the clock.
package usagehold

import (
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/usage"
)

// Rules are the failure holds, as data.
type Rules struct {
	// Transient failures (timeout, exit, output limit, an RPC error) hold
	// from Transient, doubling per consecutive failure, up to TransientMax.
	Transient, TransientMax time.Duration
	// Unavailable holds after a missing CLI or login.
	Unavailable time.Duration
	// Unsupported holds after an unverified version, a platform without
	// process groups, a reply not understood, or a run against another
	// home.
	Unsupported time.Duration
}

// DefaultRules are the holds the board uses.
var DefaultRules = Rules{
	Transient:    time.Minute,
	TransientMax: 15 * time.Minute,
	Unavailable:  5 * time.Minute,
	Unsupported:  time.Hour,
}

// Conflicts reports whether dispatches keyed a and b share pacing: same
// provider, and either one's remote account unproven or both the same. An
// unproven key conflicts with every key of its provider.
func Conflicts(a, b usage.Account) bool {
	if a.Provider != b.Provider {
		return false
	}
	if a.Remote == "" || b.Remote == "" {
		return true
	}
	return a.Remote == b.Remote
}

// HeldUntil is the earliest time a dispatch keyed k may be reserved. An
// unfinished attempt holds through its deadline plus its interval, so a
// crashed or paused dispatch still spends its interval; a finished one holds
// for its interval after completion, or its failure hold if longer. When a
// finished attempt's times disagree, its completion counts from the later
// of its finish and its deadline.
func HeldUntil(attempts []usage.Attempt, k usage.Account, rules Rules) time.Time {
	var conflicting []usage.Attempt
	for _, a := range attempts {
		if Conflicts(a.Key, k) {
			conflicting = append(conflicting, a)
		}
	}
	sort.SliceStable(conflicting, func(i, j int) bool { return conflicting[i].ReservedAt.Before(conflicting[j].ReservedAt) })
	var until time.Time
	streak := 0
	for _, a := range conflicting {
		finished, ok := a.FinishedAt.Get()
		if !ok {
			until = later(until, a.DeadlineAt.Add(a.Interval))
			continue
		}
		if !ordered(a) {
			// Its times disagree (a wall clock set back during the run):
			// it completed no earlier than its deadline, for the interval
			// and the failure hold alike.
			finished = later(finished, a.DeadlineAt)
		}
		t := finished.Add(a.Interval)
		if a.FailureCode == "" {
			streak = 0
		} else {
			streak++
			t = later(t, finished.Add(failureHold(a.FailureCode, streak, rules)))
		}
		until = later(until, t)
	}
	return until
}

// ordered reports whether a finished attempt's times agree: reserved, then
// started (when it started), then finished.
func ordered(a usage.Attempt) bool {
	finished, _ := a.FinishedAt.Get()
	if finished.Before(a.ReservedAt) {
		return false
	}
	if started, ok := a.StartedAt.Get(); ok && (started.Before(a.ReservedAt) || started.After(finished)) {
		return false
	}
	return true
}

func failureHold(code string, streak int, r Rules) time.Duration {
	switch code {
	case usage.FailNotInstalled, usage.FailNotLoggedIn:
		return r.Unavailable
	case usage.FailUnsupportedVersion, usage.FailUnsupportedPlatform, usage.FailMalformed, usage.FailHomeMismatch:
		return r.Unsupported
	}
	d := r.Transient
	for i := 1; i < streak && d < r.TransientMax; i++ {
		d *= 2
	}
	return min(d, r.TransientMax)
}

// Next picks the home of provider to collect at now: among the homes not
// held, the one whose newest attempt is oldest, a home never attempted
// first, ties in the given order. Each candidate is keyed unproven, since a
// new source instance has not yet proven its account.
func Next(provider string, homes []string, attempts []usage.Attempt, now time.Time, rules Rules) (string, bool) {
	best, found := "", false
	var bestAt time.Time
	for _, h := range homes {
		k := usage.Account{Provider: provider, Home: h}
		if now.Before(HeldUntil(attempts, k, rules)) {
			continue
		}
		var last time.Time
		for _, a := range attempts {
			if a.Key.Provider == provider && a.Key.Home == h {
				last = later(last, a.ReservedAt)
			}
		}
		if !found || last.Before(bestAt) {
			best, bestAt, found = h, last, true
		}
	}
	return best, found
}

// keep bounds the journal: attempts per home, and how long a finished one
// is kept for the failure streak.
const (
	keepPerHome = 8
	keepFor     = 24 * time.Hour
)

// Prune is the attempts the journal keeps at now: every unfinished one
// whose hold has not been over for keepFor, and the newest finished ones per
// home until keepFor after both their finish and their reservation bound.
func Prune(attempts []usage.Attempt, now time.Time) []usage.Attempt {
	sorted := append([]usage.Attempt{}, attempts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ReservedAt.After(sorted[j].ReservedAt) })
	count := map[usage.Account]int{}
	var out []usage.Attempt
	for _, a := range sorted {
		home := usage.Account{Provider: a.Key.Provider, Home: a.Key.Home}
		finished, done := a.FinishedAt.Get()
		if !done && now.Sub(a.DeadlineAt.Add(a.Interval)) > keepFor {
			continue
		}
		if done && (count[home] >= keepPerHome || now.Sub(later(finished, a.DeadlineAt.Add(a.Interval))) > keepFor) {
			continue
		}
		count[home]++
		out = append(out, a)
	}
	return out
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
