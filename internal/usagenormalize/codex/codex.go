// SPDX-License-Identifier: MIT
// Window meaning by duration, not slot, and null windows kept null follow
// CodexBar's Codex rate window normalizer and app-server snapshot builder;
// see third_party/codexbar/NOTICE.md.

// Package codex gives the Codex app-server replies their meaning: plan
// windows named by duration, credits and spend caps as exact decimals, the
// plan bucket, and the account identity the same run proved.
package codex

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/usageformat/codexrpc"
	"github.com/avivsinai/agent-message-queue/usage"
)

// Source names the app-server source in fact evidence.
const Source = "codex-app-server"

// Window durations that carry a name, in minutes.
const (
	minutes5h     = 5 * 60
	minutesWeekly = 7 * 24 * 60
)

// Normalize turns one run's replies against home into facts. The run is one
// source instance: its initialize reply must name home, and its remote
// account is resolved only when the same run's account/read reported a
// ChatGPT login and its rate limit reply named the account. A slot reported
// null stays a null window; nothing is filled from another observation.
func Normalize(r codexrpc.Replies, home string, ev usage.Evidence) usage.Result {
	acct := usage.Account{Provider: usage.ProviderCodex, Home: home}
	fail := func(code, detail string) usage.Result {
		return usage.Result{Failures: []usage.Fact[usage.Failure]{{Account: acct, Value: usage.Failure{Code: code, Detail: detail}, Evidence: ev}}}
	}
	switch init := r.Initialize; {
	case init.Err != nil:
		return fail(usage.FailRPC, "initialize error "+strconv.Itoa(init.Err.Code))
	case init.Presence != usage.Present:
		return fail(usage.FailMalformed, "no initialize reply")
	default:
		got, ok := init.Value.CodexHome.Get()
		if !ok {
			return fail(usage.FailMalformed, "initialize did not name its Codex home")
		}
		if filepath.Clean(got) != filepath.Clean(home) {
			return fail(usage.FailHomeMismatch, "app-server ran against another Codex home")
		}
	}

	var out usage.Result
	proven := false
	if a := r.Account; a.Presence == usage.Present {
		info, ok := a.Value.Account.Get()
		if a.Value.Account.Presence == usage.Null {
			return fail(usage.FailNotLoggedIn, "Codex is not logged in")
		}
		if ok {
			proven = info.Type.Value == "chatgpt" && info.Type.Presence == usage.Present
			out.Identities = append(out.Identities, usage.Fact[usage.Identity]{Account: acct, Value: usage.Identity{Type: info.Type, Plan: info.Plan}, Evidence: ev})
		}
	}

	rl := r.RateLimits
	switch {
	case rl.Err != nil:
		return usage.Merge(out, fail(usage.FailRPC, "account/rateLimits/read error "+strconv.Itoa(rl.Err.Code)))
	case rl.Presence == usage.Absent:
		return usage.Merge(out, fail(usage.FailMalformed, "no rate limit reply"))
	case rl.Presence != usage.Present:
		return usage.Merge(out, fail(usage.FailMalformed, "rate limit reply not understood"))
	}
	if id, ok := rl.Value.AccountID.Get(); ok && proven {
		acct.Remote = usage.RemoteKey(usage.ProviderCodex, id)
		for i := range out.Identities {
			out.Identities[i].Account = acct
		}
	}
	for _, s := range rl.Value.Snapshots {
		out.Windows = append(out.Windows,
			usage.Fact[usage.Window]{Account: acct, Value: window(s.LimitID, "primary", s.Primary), Evidence: ev},
			usage.Fact[usage.Window]{Account: acct, Value: window(s.LimitID, "secondary", s.Secondary), Evidence: ev})
		if c, ok := s.Credits.Get(); ok {
			out.Money = append(out.Money, usage.Fact[usage.Money]{Account: acct, Evidence: ev, Value: usage.Money{
				LimitID: s.LimitID, Kind: usage.MoneyBalance, Unit: "credits",
				Amount: c.Balance, Unlimited: c.Unlimited,
			}})
		}
		if l, ok := s.Individual.Get(); ok {
			out.Money = append(out.Money, usage.Fact[usage.Money]{Account: acct, Evidence: ev, Value: usage.Money{
				LimitID: s.LimitID, Kind: usage.MoneyCap,
				Amount: l.Used, Limit: l.Limit, RemainingPercent: l.RemainingPercent,
				ResetsAt: unix(l.ResetsAt),
			}})
		}
		out.Buckets = append(out.Buckets, usage.Fact[usage.Bucket]{Account: acct, Evidence: ev, Value: usage.Bucket{
			LimitID: s.LimitID, Plan: s.Plan, Reached: s.Reached,
		}})
	}
	return out
}

func window(limitID, slot string, f usage.Field[codexrpc.Window]) usage.Window {
	w := usage.Window{LimitID: limitID, Slot: slot, Reported: f.Presence}
	v, ok := f.Get()
	if !ok {
		return w
	}
	w.DurationMinutes = v.WindowDurationMins
	w.UsedPercent = v.UsedPercent
	w.ResetsAt = unix(v.ResetsAt)
	if d, ok := v.WindowDurationMins.Get(); ok {
		switch d {
		case minutes5h:
			w.Kind = usage.Window5h
		case minutesWeekly:
			w.Kind = usage.WindowWeekly
		}
	}
	return w
}

func unix(f usage.Field[int64]) usage.Field[time.Time] {
	if s, ok := f.Get(); ok {
		return usage.Some(time.Unix(s, 0).UTC())
	}
	return usage.Field[time.Time]{Presence: f.Presence}
}
