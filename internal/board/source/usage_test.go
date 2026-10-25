package source_test

import (
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// The session list's conversations[] attribute each harness session's spend
// to its worker: subagent and child-thread spend rolls up to the mapped
// root, a revoked record still attributes, an uncertain one does not, and
// account limits stay off the chips except a reached limit.
func TestBoardAttributesUsageThroughSessionConversations(t *testing.T) {
	m := runBoard(t, fakeRunner{t: t, outputs: outputs("thread-open.json")}, true)
	mapping := board.UsageMapping(m.Conversations)
	cfg := usage.Config{Homes: map[string][]string{
		usage.Claude: {"../usage/testdata/claude"},
		usage.Codex:  {"../usage/testdata/codex"},
		usage.Pi:     {"../usage/testdata/pi"},
	}}
	rep := usage.Read(cfg, m.GeneratedAt, []string{"/work/demo"}, mapping)
	m = board.WithUsage(m, rep, mapping)

	type want struct{ fresh, cached int64 }
	wants := map[[2]string]want{
		// msg_demo_a counted once (written twice) plus the subagent line;
		// the 5 h window aligns to the rejection's reset at 14:10.
		{"s1", "alpha"}: {fresh: 2 + 275 + 650 + 2 + 35104 + 7, cached: 801103 + 13991},
		// Root thread deltas after 07:00 (the repeated total adds nothing)
		// plus the spawned child thread.
		{"s1", "beta"}: {fresh: (800 + 200) + (1000 + 200) + (500 + 50), cached: 3200 + 3000 + 1500},
	}
	chips := map[[2]string]board.RosterAgent{}
	for _, rs := range m.Roster {
		for _, a := range append(append([]board.RosterAgent{}, rs.Agents...), rs.Quiet...) {
			chips[[2]string{a.Session, a.Handle}] = a
		}
	}
	for key, w := range wants {
		a := chips[key]
		if len(a.Usage) != 2 || a.Usage[0].Window != usage.Window5h {
			t.Fatalf("%v usage = %+v, want 5h and 7d", key, a.Usage)
		}
		if got := a.Usage[0]; got.Fresh != w.fresh || got.CacheRead != w.cached {
			t.Errorf("%v 5h = %d fresh, %d cached; want %d, %d", key, got.Fresh, got.CacheRead, w.fresh, w.cached)
		}
	}
	if a := chips[[2]string{"s1", "alpha"}]; a.LimitReached == nil || a.LimitReached.ResetsAt == nil || a.LimitReached.ResetsAt.UTC() != time.Date(2026, 10, 22, 14, 10, 0, 0, time.UTC) {
		t.Errorf("alpha limit reached = %+v, want the five_hour rejection until 14:10Z", a.LimitReached)
	}
	if a := chips[[2]string{"s2", "alpha"}]; a.Usage != nil {
		t.Errorf("uncertain record attributed spend to s2/alpha: %+v", a.Usage)
	}

	accounts := map[string]board.AccountUsage{}
	for _, a := range m.Usage.Accounts {
		accounts[a.Harness] = a
	}
	codex := accounts[usage.Codex]
	if len(codex.Limits) == 0 || codex.Limits[0].Kind != "credits" || codex.Limits[0].Balance != "118.0" {
		t.Errorf("codex limits = %+v, want the newest credit balance 118.0", codex.Limits)
	}
	if pi := accounts[usage.Pi]; pi.OtherSessions != 1 || pi.Other[0].Fresh != 150 || pi.Other[0].CostUSD != nil {
		t.Errorf("pi other = %d sessions, %+v; want 1 session, 150 fresh, no cost", pi.OtherSessions, pi.Other)
	}

	var b strings.Builder
	if err := board.WriteText(&b, m); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, s := range []string{
		"Claude ../usage/testdata/claude: 5 h limit reached",
		"Codex ../usage/testdata/codex: 118.0 credits",
		"limit reached until",
		"cost not reported",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("text lacks %q:\n%s", s, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "      ") && strings.Contains(line, "credits") {
			t.Errorf("a chip shows the account balance: %q", line)
		}
	}
}
