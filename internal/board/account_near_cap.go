package board

import (
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
	facts "github.com/avivsinai/agent-message-queue/usage"
)

// accountNearCapThreshold is the used share of a cap or window at which an
// account becomes a floor problem: 90 percent used, 10 percent left.
const accountNearCapThreshold = 90.0

// accountNearCap reports one problem per account whose freshest cap or
// window reading, not stale by the plan-limits 15-minute rule, shows 90
// percent or more used. It needs no mapped worker and no card: the fact is
// the account's. A stale reading says nothing about now and raises nothing.
// Harnesses that report nothing before the limit (Claude Code, pi) have no
// such fact and stay silent.
func accountNearCap(m Model, now time.Time) []Problem {
	if m.Usage == nil {
		return nil
	}
	var out []Problem
	for _, a := range m.Usage.Accounts {
		l := nearCapLimit(a.Limits, now)
		if l == nil {
			continue
		}
		resets := "an unknown time"
		if l.ResetsAt != nil {
			resets = resetTime(*l.ResetsAt, now)
		}
		out = append(out, Problem{
			Kind: ProblemAccountNearCap, Account: a.Key,
			ResetsAt: l.ResetsAt,
			Text: fmt.Sprintf("account %s: %s of %s used (%s left), resets %s",
				accountName(a.Key), nearCapUsed(l), nearCapTotal(l), nearCapLeft(l), resets),
		})
	}
	return out
}

// nearCapLimit picks the account's freshest non-stale cap or window limit at
// or past the threshold, or nil.
func nearCapLimit(ls []usage.Limit, now time.Time) *usage.Limit {
	var best *usage.Limit
	for i := range ls {
		l := &ls[i]
		if l.Kind != "cap" && l.Kind != "window" {
			continue
		}
		// A stale reading is old news: the 15-minute plan-limits rule says
		// it must not present as current. A reading with no status is not
		// an app-server reading and carries no usable freshness.
		if l.Status != string(facts.StatusFresh) && l.Status != string(facts.StatusPartial) {
			continue
		}
		if l.UsedPercent == nil && l.RemainingPercent == nil {
			continue
		}
		if nearCapUsedPercent(l) < accountNearCapThreshold {
			continue
		}
		if best == nil || l.AsOf.After(best.AsOf) {
			best = l
		}
	}
	return best
}

// nearCapUsedPercent is the reading's used share: the reported used percent,
// else 100 minus the reported remaining percent.
func nearCapUsedPercent(l *usage.Limit) float64 {
	if l.UsedPercent != nil {
		return *l.UsedPercent
	}
	return 100 - *l.RemainingPercent
}

// nearCapUsed prints the used figure: a cap's exact decimals, else the
// window's percent.
func nearCapUsed(l *usage.Limit) string {
	if l.Used != "" {
		return amount(l.Used)
	}
	return percent(nearCapUsedPercent(l))
}

// nearCapTotal prints the limit figure: a cap's exact decimals, else 100%.
func nearCapTotal(l *usage.Limit) string {
	if l.Cap != "" {
		return amount(l.Cap)
	}
	return "100%"
}

// nearCapLeft prints what the reading says is left.
func nearCapLeft(l *usage.Limit) string {
	if l.RemainingPercent != nil {
		return percent(*l.RemainingPercent)
	}
	return percent(100 - nearCapUsedPercent(l))
}
