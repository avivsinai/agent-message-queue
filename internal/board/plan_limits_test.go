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
	rep := usage.Report{Now: now, Accounts: []usage.Account{{
		Key: "codex:~/.codex", Harness: usage.Codex, Home: home,
		Limits: []usage.Limit{
			{Kind: "credits", Name: "credits", Balance: "3.0", AsOf: now.Add(-time.Hour), Source: "rollout"},
			{Kind: "unknown", Name: "windows", Source: "rollout", Reason: "windows not reported by this plan"},
		},
	}}}
	acct := facts.Account{Provider: facts.ProviderCodex, Home: home}
	ev := facts.Evidence{Source: "codex-app-server", ObservedAt: now.Add(-time.Minute)}
	window := func(slot, kind string, mins int64, used float64) facts.Fact[facts.Window] {
		return facts.Fact[facts.Window]{Account: acct, Evidence: ev, Value: facts.Window{
			LimitID: "codex", Slot: slot, Reported: facts.Present, Kind: kind,
			DurationMinutes: facts.Some(mins), UsedPercent: facts.Some(used), ResetsAt: facts.Some(now.Add(2 * time.Hour)),
		}}
	}
	r := facts.Result{
		Windows: []facts.Fact[facts.Window]{window("primary", facts.WindowWeekly, 10080, 12), window("secondary", facts.Window5h, 300, 37)},
		Money: []facts.Fact[facts.Money]{{Account: acct, Evidence: ev, Value: facts.Money{
			LimitID: "codex", Kind: facts.MoneyBalance, Unit: "credits", Amount: facts.Some("209.7055460214615"),
		}}},
	}

	got := WithPlanLimits(rep, r, now).Accounts[0].Limits
	line := limitsLine(got, now)
	resets := now.Add(2 * time.Hour).Local().Format("15:04")
	want := "5 h 37% used, resets " + resets + " · weekly 12% used, resets " + resets + " · 209.7 credits · read 1m ago"
	if line != want {
		t.Fatalf("strip = %q\nwant    %q", line, want)
	}
	for _, l := range got {
		if l.Source != appServer || l.Status != string(facts.StatusFresh) {
			t.Errorf("limit %+v kept a rollout reading or lost its status", l)
		}
		if l.Kind == "credits" && l.Balance != "209.7055460214615" {
			t.Errorf("credits balance %q not kept verbatim", l.Balance)
		}
	}
	if strings.Contains(line, "not reported by this plan") {
		t.Errorf("rollout guess kept beside the app-server reading: %q", line)
	}
}
