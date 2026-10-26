package board

import (
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
	facts "github.com/avivsinai/agent-message-queue/usage"
)

func TestPlanLimitsReplaceRolloutLimitsOnTheStrip(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	home := "/home/u/.codex"
	acct := facts.Account{Provider: facts.ProviderCodex, Home: home}
	ev := facts.Evidence{Source: "codex-app-server", ObservedAt: now.Add(-time.Minute)}
	window := func(slot, kind string, mins int64, used float64) facts.Fact[facts.Window] {
		return facts.Fact[facts.Window]{Account: acct, Evidence: ev, Value: facts.Window{
			LimitID: "codex", Slot: slot, Reported: facts.Present, Kind: kind,
			DurationMinutes: facts.Some(mins), UsedPercent: facts.Some(used), ResetsAt: facts.Some(now.Add(2 * time.Hour)),
		}}
	}
	balance := func(amount facts.Field[string]) []facts.Fact[facts.Money] {
		return []facts.Fact[facts.Money]{{Account: acct, Evidence: ev, Value: facts.Money{
			LimitID: "codex", Kind: facts.MoneyBalance, Unit: "credits", Amount: amount, Unlimited: facts.Some(false),
		}}}
	}
	windows := []facts.Fact[facts.Window]{window("primary", facts.WindowWeekly, 10080, 12), window("secondary", facts.Window5h, 300, 37)}
	resets := now.Add(2 * time.Hour).Local().Format("15:04")
	for _, tc := range []struct {
		name   string
		r      facts.Result
		want   string
		status facts.Status
	}{
		{
			name:   "windows and credits",
			r:      facts.Result{Windows: windows, Money: balance(facts.Some("209.7055460214615"))},
			want:   "5 h 37% used, resets " + resets + " · weekly 12% used, resets " + resets + " · 209.7 credits · read 1m ago",
			status: facts.StatusFresh,
		},
		{
			// Seen live with codex 0.163.0: a reply whose credits carried
			// "balance": null printed " credits" with no number.
			name:   "null balance",
			r:      facts.Result{Windows: windows, Money: balance(facts.Field[string]{Presence: facts.Null})},
			want:   "5 h 37% used, resets " + resets + " · weekly 12% used, resets " + resets + " · credit balance not reported in the app-server reply · read 1m ago",
			status: facts.StatusFresh,
		},
		{
			// PR #1235 review: the collector can return valid facts and
			// a transport or cache-write failure from the same observation.
			name: "failed read with usable facts",
			r: facts.Result{Windows: windows, Money: balance(facts.Some("209.7055460214615")),
				Failures: []facts.Fact[facts.Failure]{{Account: acct, Evidence: ev,
					Value: facts.Failure{Code: facts.FailTimeout, Detail: "command timed out"}}}},
			want:   "5 h 37% used, resets " + resets + " · weekly 12% used, resets " + resets + " · 209.7 credits · last app-server read failed (command timed out) · read 1m ago",
			status: facts.StatusPartial,
		},
		{
			// usage slice review finding 6: a cap without its amounts
			// printed "spend cap  of  used", and an unknown balance lost
			// its bucket name.
			name: "cap and balance not reported",
			r: facts.Result{Windows: windows, Money: []facts.Fact[facts.Money]{
				{Account: acct, Evidence: ev, Value: facts.Money{LimitID: "other", Kind: facts.MoneyBalance, Unit: "credits",
					Amount: facts.Field[string]{Presence: facts.Null}, Unlimited: facts.Some(false)}},
				{Account: acct, Evidence: ev, Value: facts.Money{LimitID: "codex", Kind: facts.MoneyCap,
					Amount: facts.Some("12.5"), Limit: facts.Field[string]{Presence: facts.Null}}},
			}},
			want: "5 h 37% used, resets " + resets + " · weekly 12% used, resets " + resets +
				" · other credit balance not reported in the app-server reply · spend cap not reported in the app-server reply · 3.0 credits as of 1h ago · read 1m ago",
			status: facts.StatusFresh,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := usage.Report{Now: now, Accounts: []usage.Account{{
				Key: "codex:~/.codex", Harness: usage.Codex, Home: home,
				Limits: []usage.Limit{
					{Kind: "credits", Name: "credits", Balance: "3.0", AsOf: now.Add(-time.Hour), Source: "rollout"},
					{Kind: "unknown", Name: "windows", Source: "rollout", Reason: "windows not reported by this plan"},
				},
			}}}
			got := WithPlanLimits(rep, tc.r, now).Accounts[0].Limits
			if line := limitsLine(got, now); line != tc.want {
				t.Fatalf("strip = %q\nwant    %q", line, tc.want)
			}
			for _, l := range got {
				if tc.name == "cap and balance not reported" && l.Kind == "credits" && l.Bucket == "" {
					if l.Balance != "3.0" || l.Source != "rollout" {
						t.Errorf("unreported app-server balance replaced the rollout: %+v", l)
					}
					continue
				}
				if l.Source != appServer || l.Status != string(tc.status) {
					t.Errorf("limit %+v kept a rollout reading or lost its status", l)
				}
				if l.Kind == "credits" && l.Balance != "209.7055460214615" {
					t.Errorf("credits balance %q not kept verbatim", l.Balance)
				}
				if strings.Contains(l.Reason, "not reported by this plan") {
					t.Errorf("rollout guess kept beside the app-server reading: %+v", l)
				}
			}
		})
	}
}

// post-release review v0.113.2 finding U2: a week-old app-server snapshot
// replaced one-minute-old rollout limits and flagged the account with an
// undated "reached". Review finding: a recent snapshot also replaced a
// newer rollout balance and reached state. Keep its windows, but choose
// overlapping facts by their own observation times.
func TestPlanLimitsReplaceRolloutOnlyWhenTheSnapshotIsNotStale(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	home := "/home/u/.codex"
	acct := facts.Account{Provider: facts.ProviderCodex, Home: home}
	for _, tc := range []struct {
		name     string
		age      time.Duration
		appLimit bool // the app-server reading is on the strip
	}{
		{"fresh windows with older balance and reached facts", 10 * time.Second, true},
		{"stale snapshot", 7 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := facts.Evidence{Source: "codex-app-server", ObservedAt: now.Add(-tc.age)}
			older := ev
			if tc.appLimit {
				older.ObservedAt = now.Add(-2 * time.Minute)
			}
			r := facts.Result{
				Windows: []facts.Fact[facts.Window]{{Account: acct, Evidence: ev, Value: facts.Window{
					LimitID: "codex", Slot: "primary", Reported: facts.Present, Kind: facts.Window5h,
					DurationMinutes: facts.Some[int64](300), UsedPercent: facts.Some(42.0)}}},
				Buckets: []facts.Fact[facts.Bucket]{{Account: acct, Evidence: older,
					Value: facts.Bucket{LimitID: "codex", Reached: facts.Some("primary")}}},
				Money: []facts.Fact[facts.Money]{{Account: acct, Evidence: older,
					Value: facts.Money{LimitID: "codex", Kind: facts.MoneyBalance, Amount: facts.Some("10.0")}}},
			}
			rollout := []usage.Limit{
				{Kind: "credits", Name: "credits", Balance: "3.0", AsOf: now.Add(-20 * time.Second), Source: "rollout"},
				{Kind: "reached", Name: "secondary", AsOf: now.Add(-20 * time.Second), Source: "rollout"},
			}
			rep := usage.Report{Now: now, Accounts: []usage.Account{{Key: "codex:~/.codex", Harness: usage.Codex, Home: home, Limits: rollout}}}
			app, credits, reached := false, false, false
			for _, l := range WithPlanLimits(rep, r, now).Accounts[0].Limits {
				app = app || l.Source == appServer && l.Kind == "window"
				credits = credits || l.Kind == "credits" && l.Balance == "3.0" && l.Source == "rollout"
				reached = reached || l.Kind == "reached" && l.Name == "secondary" && l.Source == "rollout"
				if !tc.appLimit && l.Source == appServer {
					t.Fatalf("a stale app-server reading replaced rollout limits: %+v", l)
				}
			}
			if app != tc.appLimit || !credits || !reached {
				t.Fatalf("app-server windows %v (want %v), newer rollout credits %v, reached %v", app, tc.appLimit, credits, reached)
			}
		})
	}
}

// post-release review v0.113.2 finding U1 follow-up: a recovered journal
// marked every account "last app-server read failed" beside readings made in
// the same pass. Usage re-check of d19eaa3a: the skip also hid a journal
// that cannot be read, which stops every pass.
func TestPlanLimitsShowOnlyBlockingJournalFailures(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	home := "/home/u/.codex"
	acct := facts.Account{Provider: facts.ProviderCodex, Home: home}
	ev := facts.Evidence{Source: "codex-app-server", ObservedAt: now.Add(-time.Minute)}
	for _, tc := range []struct {
		name  string
		code  string
		shown bool
	}{
		{"recovered journal", facts.FailJournalRecovered, false},
		{"unreadable journal", facts.FailJournal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := facts.Result{
				Windows: []facts.Fact[facts.Window]{{Account: acct, Evidence: ev, Value: facts.Window{
					LimitID: "codex", Slot: "primary", Reported: facts.Present, Kind: facts.Window5h,
					DurationMinutes: facts.Some[int64](300), UsedPercent: facts.Some(42.0)}}},
				Failures: []facts.Fact[facts.Failure]{{Value: facts.Failure{Code: tc.code, Detail: "journal detail"}}},
			}
			rep := usage.Report{Now: now, Accounts: []usage.Account{{Key: "codex:~/.codex", Harness: usage.Codex, Home: home}}}
			shown := false
			for _, l := range WithPlanLimits(rep, r, now).Accounts[0].Limits {
				shown = shown || strings.Contains(l.Reason, "journal detail")
			}
			if shown != tc.shown {
				t.Fatalf("journal failure shown = %v, want %v", shown, tc.shown)
			}
		})
	}
}
