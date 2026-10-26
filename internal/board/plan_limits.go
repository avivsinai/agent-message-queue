package board

import (
	"slices"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
	facts "github.com/avivsinai/agent-message-queue/usage"
)

// appServer names the Codex app-server as a limit's source.
const appServer = "app-server"

// LimitsStaleAfter is the age past which a plan limit reading is stale.
const LimitsStaleAfter = 15 * time.Minute

// WithPlanLimits gives each Codex account the plan limits the Codex
// app-server reported for the same home. Windows come from the app-server;
// overlapping balances and reached states keep the newer observation. A
// home with no usable app-server reading keeps its rollout limits. It is
// pure; now dates each reading's status.
func WithPlanLimits(rep usage.Report, r facts.Result, now time.Time) usage.Report {
	out := rep
	out.Accounts = append([]usage.Account{}, rep.Accounts...)
	for i := range out.Accounts {
		a := &out.Accounts[i]
		if a.Harness == usage.Codex {
			a.Limits = planLimits(a.Limits, r, a.Home, now)
		}
	}
	return out
}

func planLimits(rollout []usage.Limit, r facts.Result, home string, now time.Time) []usage.Limit {
	f := r.ForHome(facts.ProviderCodex, home)
	as := facts.Assess(r, facts.ProviderCodex, home, now, LimitsStaleAfter)
	failure := as.Failure
	if failure == nil {
		// A failure of the collection itself names no provider and
		// concerns every home.
		for _, x := range r.ForHome("", "").Failures {
			// A recovered journal is a diagnostic of the collector, not a
			// failed read: it does not mark readings made in the same pass.
			if x.Value.Code == facts.FailJournal && as.Status != facts.StatusUnknown {
				continue
			}
			v := x.Value
			failure = &v
		}
	}
	if as.Status == facts.StatusUnknown {
		if failure == nil {
			return rollout
		}
		return append(append([]usage.Limit{}, rollout...), usage.Limit{
			Kind: "unknown", Name: "plan limits", Source: appServer,
			Reason: "plan limits not read (" + failure.Detail + ")",
		})
	}
	if as.Status == facts.StatusStale && len(rollout) > 0 {
		// A stale snapshot must not replace the rollout readings or carry
		// its old flags onto the account. Rollouts hold no windows, so a
		// snapshot inside the freshness bound is the better reading even
		// when a rollout reading is newer.
		if failure == nil {
			return rollout
		}
		return append(append([]usage.Limit{}, rollout...), usage.Limit{
			Kind: "unknown", Name: "plan limits", Source: appServer,
			Reason: "plan limits not read (" + failure.Detail + ")",
		})
	}
	status := string(as.Status)
	base := func(kind, name, limitID string) usage.Limit {
		return usage.Limit{Kind: kind, Name: name, Bucket: bucketName(limitID), AsOf: as.ObservedAt, Source: appServer, Status: status}
	}

	windows := append([]facts.Fact[facts.Window]{}, f.Windows...)
	sort.SliceStable(windows, func(i, j int) bool {
		di, oki := windows[i].Value.DurationMinutes.Get()
		dj, okj := windows[j].Value.DurationMinutes.Get()
		if oki != okj {
			return oki
		}
		return di < dj
	})
	var out []usage.Limit
	for _, wf := range windows {
		w := wf.Value
		if w.Reported != facts.Present {
			continue
		}
		l := base("window", w.Kind, w.LimitID)
		l.AsOf = wf.Evidence.ObservedAt
		if p, ok := w.UsedPercent.Get(); ok {
			l.UsedPercent = &p
		}
		if d, ok := w.DurationMinutes.Get(); ok {
			l.WindowMinutes = d
		}
		if t, ok := w.ResetsAt.Get(); ok {
			l.ResetsAt = &t
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		l := base("unknown", "windows", "")
		l.Reason = "windows not reported in the app-server reply"
		out = append(out, l)
	}
	for _, b := range f.Buckets {
		if reached, ok := b.Value.Reached.Get(); ok {
			l := base("reached", reached, b.Value.LimitID)
			l.AsOf = b.Evidence.ObservedAt
			out = append(out, l)
		}
	}
	for _, mf := range f.Money {
		m := mf.Value
		switch m.Kind {
		case facts.MoneyBalance:
			l := base("credits", "credits", m.LimitID)
			l.Unlimited = m.Unlimited.Value && m.Unlimited.Presence == facts.Present
			l.Balance = m.Amount.Value
			if !l.Unlimited && m.Amount.Presence != facts.Present {
				l = base("unknown", "credits", m.LimitID)
				l.Reason = "credit balance not reported in the app-server reply"
			}
			l.AsOf = mf.Evidence.ObservedAt
			out = append(out, l)
		case facts.MoneyCap:
			if m.Amount.Presence != facts.Present || m.Limit.Presence != facts.Present {
				l := base("unknown", "spend cap", m.LimitID)
				l.AsOf = mf.Evidence.ObservedAt
				l.Reason = "spend cap not reported in the app-server reply"
				out = append(out, l)
				continue
			}
			l := base("cap", "spend cap", m.LimitID)
			l.AsOf = mf.Evidence.ObservedAt
			l.Used, l.Cap = m.Amount.Value, m.Limit.Value
			if p, ok := m.RemainingPercent.Get(); ok {
				l.RemainingPercent = &p
			}
			if t, ok := m.ResetsAt.Get(); ok {
				l.ResetsAt = &t
			}
			out = append(out, l)
		}
	}
	out = mergeRolloutLimits(out, rollout, f.Buckets)
	if failure != nil {
		l := base("unknown", "plan limits", "")
		l.Reason = "last app-server read failed (" + failure.Detail + ")"
		out = append(out, l)
	}
	return out
}

// mergeRolloutLimits keeps observations of the same capability and bucket
// ordered by their own time: an unreported app-server balance replaces only
// an older balance of its own bucket. A newer bucket reading can clear an earlier
// rollout reached state even when it reports no reached limit.
func mergeRolloutLimits(out, rollout []usage.Limit, buckets []facts.Fact[facts.Bucket]) []usage.Limit {
	for _, l := range rollout {
		if l.Kind != "credits" && l.Kind != "reached" {
			continue
		}
		i := slices.IndexFunc(out, func(other usage.Limit) bool {
			kind := other.Kind
			if kind == "unknown" && other.Name == "credits" {
				kind = "credits"
			}
			return kind == l.Kind && other.Bucket == l.Bucket
		})
		if i >= 0 {
			if l.AsOf.After(out[i].AsOf) {
				out[i] = l
			}
			continue
		}
		if l.Kind == "reached" && slices.ContainsFunc(buckets, func(b facts.Fact[facts.Bucket]) bool {
			return bucketName(b.Value.LimitID) == l.Bucket && !l.AsOf.After(b.Evidence.ObservedAt)
		}) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// bucketName is a limit's bucket when it is not Codex's main one.
func bucketName(limitID string) string {
	if limitID == "codex" {
		return ""
	}
	return limitID
}
