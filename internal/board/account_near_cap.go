package board

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// accountNearCapThreshold is the used share of a cap at which an account
// becomes a floor problem: 90 percent used, 10 percent left.
const accountNearCapThreshold = 90.0

// accountNearCap reports one problem per account whose freshest cap reading,
// fresh by its own time within the plan-limits 15-minute rule, shows 90
// percent or more used. The newest fact for a cap decides before the
// threshold: an older 95-percent reading beside a newer 50-percent one
// raises nothing. It needs no mapped worker and no card: the fact is the
// account's. Harnesses that report nothing before the limit (Claude Code,
// pi) have no such fact and stay silent.
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
		resets := ""
		if l.ResetsAt != nil {
			resets = ", resets " + resetTime(*l.ResetsAt, now)
		}
		// The threshold check already proved the cap has a usable reading.
		p, _ := nearCapUsedPercent(l)
		used, total := capAmount(l.Used), capAmount(l.Cap)
		if used == "" {
			used = percent(p)
		}
		if total == "" {
			total = "100%"
		}
		out = append(out, Problem{
			Kind: ProblemAccountNearCap, Account: a.Key,
			ResetsAt: l.ResetsAt,
			Text: fmt.Sprintf("account %s: %s of %s used (%s left)%s",
				accountName(a.Key), used, total, nearCapLeftText(p), resets),
		})
	}
	return out
}

// nearCapLimit picks the account's problem cap, or nil. The freshest fresh
// fact for each cap decides whether that cap passes the threshold.
func nearCapLimit(ls []usage.Limit, now time.Time) *usage.Limit {
	freshest := make(map[string]*usage.Limit)
	for i := range ls {
		l := &ls[i]
		if l.Kind != "cap" || !capFresh(l, now) {
			continue
		}
		key := l.Name + "\x00" + l.Bucket
		if cur, ok := freshest[key]; !ok || l.AsOf.After(cur.AsOf) {
			freshest[key] = l
		}
	}
	var best *usage.Limit
	for i := range ls {
		l := &ls[i]
		if freshest[l.Name+"\x00"+l.Bucket] != l {
			continue
		}
		p, ok := nearCapUsedPercent(l)
		if !ok || p < accountNearCapThreshold {
			continue
		}
		if best == nil || l.AsOf.After(best.AsOf) {
			best = l
		}
	}
	return best
}

// capFresh says whether a cap's own reading time is current: set, not in the
// future, and within the plan-limits freshness bound. The limit's status
// field and the account's other readings play no part.
func capFresh(l *usage.Limit, now time.Time) bool {
	if l.AsOf.IsZero() || l.AsOf.After(now) {
		return false
	}
	return now.Sub(l.AsOf) <= LimitsStaleAfter
}

// capUnits parses a cap's exact units as finite decimals.
func capUnits(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// nearCapUsedPercent is the cap's used share: used over cap when both exact
// unit strings parse as finite decimals and the cap is positive, else the
// reported used percent, else 100 minus the reported remaining percent. A
// cap with no usable reading of any kind has no used share.
func nearCapUsedPercent(l *usage.Limit) (float64, bool) {
	if u, ok := capUnits(l.Used); ok {
		if c, ok := capUnits(l.Cap); ok && c > 0 {
			return u / c * 100, true
		}
	}
	if l.UsedPercent != nil {
		return *l.UsedPercent, true
	}
	if l.RemainingPercent != nil {
		return 100 - *l.RemainingPercent, true
	}
	return 0, false
}

// capAmount prints a cap's figure exactly as the source wrote it when it
// parses as a finite decimal; an empty or invalid figure stays empty.
func capAmount(s string) string {
	if _, ok := capUnits(s); !ok {
		return ""
	}
	return s
}

// nearCapLeftText prints what is left of the cap: never negative and
// truncated to at most one decimal.
func nearCapLeftText(usedPercent float64) string {
	left := 100 - usedPercent
	if left < 0 {
		left = 0
	}
	s := strconv.FormatFloat(left, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s) > i+2 {
		s = s[:i+2]
	}
	return s + "%"
}
