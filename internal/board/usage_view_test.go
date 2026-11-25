package board

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// liveBoardUsage reads one Claude account shaped like the live board of
// 2026-11-02: a seven_day rejection three days ago whose reset is still
// ahead, spend before the aligned 7 d window starts, and spend in the last
// hour, all by the session mapped to s1/w.
func liveBoardUsage(t *testing.T) (Model, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	const sid = "aaaaaaaa-0000-4000-8000-000000000001"
	resets := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC).Unix()
	row := func(at, id string, out int, extra string) string {
		return `{"type":"assistant","timestamp":"` + at + `","sessionId":"` + sid + `","cwd":"/work/demo","version":"2.1.300","message":{"id":"` + id + `","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":` + strconv.Itoa(out) + `}}` + extra + `}`
	}
	home := t.TempDir()
	path := filepath.Join(home, "projects", "-work-demo", sid+".jsonl")
	lines := []string{
		row("2026-10-26T18:00:00.000Z", "msg_old", 1000, ""),
		row("2026-10-30T06:34:00.000Z", "msg_rejected", 0, `,"quotaLimits":{"status":"rejected","resetsAt":`+strconv.FormatInt(resets, 10)+`,"rateLimitType":"seven_day"}`),
		row("2026-11-02T11:00:00.000Z", "msg_new", 10, ""),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapping := usage.Mapping{{Harness: usage.Claude, ID: sid}: {Session: "s1", Handle: "w"}}
	rep := usage.Read(usage.Config{Homes: map[string][]string{usage.Claude: {home}}}, now, []string{"/work/demo"}, mapping)
	m := Model{GeneratedAt: now, Roster: []RosterSession{{Session: "s1", Agents: []RosterAgent{{RosterEntry: RosterEntry{Session: "s1", Handle: "w", ValidHandle: true}}}}}}
	return WithUsage(m, rep, mapping), now
}

// Observed on the live board 2026-11-02: the fleet added Claude's 7 d
// window, aligned to the account's reset, to Codex's trailing one and
// labelled the sum "last 7 d" (35.4M against 44.5M trailing). One aligned
// account is enough: the fleet must count the trailing window, not its.
func TestFleetCountsTheTrailingWindows(t *testing.T) {
	m, now := liveBoardUsage(t)
	if w := m.Usage.Accounts[0].Workers[0].Usage[1]; w.Label != "7 d window" || w.Fresh != 10 {
		t.Fatalf("worker 7 d = %q %d fresh, want the aligned window's 10", w.Label, w.Fresh)
	}
	f := m.Usage.Fleet[1]
	if f.Label != "last 7 d" || !f.WindowFrom.Equal(now.Add(-7*24*time.Hour)) || f.Fresh != 1010 {
		t.Fatalf("fleet 7 d = %q from %v, %d fresh; want last 7 d from %v, 1010 fresh", f.Label, f.WindowFrom, f.Fresh, now.Add(-7*24*time.Hour))
	}
}

// Observed on the live board 2026-11-02: the limits line read "7 d limit
// reached, resets 05:00" and the chip "limit reached until 05:00" from a
// rejection on Oct 30, after which the account spent ~3M fresh tokens.
func TestLimitTheAccountSpentAfterReadsAsPast(t *testing.T) {
	m, now := liveBoardUsage(t)
	const want = "7 d limit reported 3d ago; the account was active since then"
	var b strings.Builder
	writeUsage(&b, m)
	if !strings.Contains(b.String(), accountName(m.Usage.Accounts[0].Key)+": "+want) {
		t.Errorf("limits text lacks %q:\n%s", want, b.String())
	}
	if chip := usageLine(m.Roster[0].Agents[0], now); !strings.HasSuffix(chip, want) {
		t.Errorf("chip = %q, want it to end %q", chip, want)
	}
}

// Observed on the live board 2026-11-02: an account that read 66 of 336
// files said "other sessions in this project (0)", as if there were none.
func TestPartialAccountCountsOtherSessionsAsALowerBound(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := Model{GeneratedAt: now, Usage: &UsageView{Accounts: []AccountUsage{{Key: "claude:~/.claude", Harness: usage.Claude, Partial: true, Detail: "partial: 66 of 336 files"}}}}
	var b strings.Builder
	writeUsage(&b, m)
	if !strings.Contains(b.String(), "other sessions in this project (at least 0)") {
		t.Errorf("text does not mark the count a lower bound:\n%s", b.String())
	}
}

// A Codex account's windows align to the reset the app-server reports, like
// Claude's, instead of trailing (audit 2026-11-10).
func TestCodexWindowsAlignToReportedReset(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	r5, r7 := now.Add(2*time.Hour), now.Add(3*24*time.Hour)
	ws := accountWindows(usage.Account{Limits: []usage.Limit{
		{Kind: "window", Name: "5h", ResetsAt: &r5},
		{Kind: "window", Name: "weekly", ResetsAt: &r7},
	}}, now)
	if !ws[0].from.Equal(r5.Add(-5*time.Hour)) || ws[0].label != "5 h window" ||
		!ws[1].from.Equal(r7.Add(-7*24*time.Hour)) || ws[1].label != "7 d window" {
		t.Fatalf("windows = %+v, want both aligned to the reported resets", ws)
	}
}

// An app-server "reached" limit the account spent tokens after reads as past
// in the limits line, as it does on the chip (audit 2026-11-10).
func TestLimitsLineReadsPastAppServerLimitAsPast(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	l := usage.Limit{Kind: "reached", Name: "seven_day", Source: appServer, Past: true, AsOf: now.Add(-3 * 24 * time.Hour)}
	got := limitsLine([]usage.Limit{l}, now)
	if !strings.Contains(got, "limit reported 3d ago; the account was active since then") {
		t.Fatalf("limitsLine = %q, want the past form", got)
	}
}

// Each seat shows one context line: the latest response's prompt size with
// its age, and the 5 h cache-read share when the window holds cache reads
// (pi records none). Origin: Field observation, 2026-11-25: a seat's latest
// response carried an 888K-token context and the board showed only window
// sums. Measured facts, not advice; no threshold. The text face renders it
// as its own indented line under the seat's usage line, the HTML face as
// its own paragraph in the Usage details.
func TestSeatShowsContextSizeAndCacheReadShare(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	row := func(agent RosterAgent, want string) {
		t.Helper()
		if got := contextLine(agent, now); got != want {
			t.Errorf("contextLine = %q, want %q", got, want)
		}
	}
	// A Claude lead: the newest response carried 887,765 prompt tokens,
	// 99.7% of them cache reads, one hour ago.
	lead := RosterAgent{Usage: []Usage{
		{Window: usage.Window5h, In: 1294337, CacheRead: 38630586, Fresh: 1394337, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)},
		{Window: usage.Window7d, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)},
	}}
	row(lead, "context 887.8K at 1h ago · cache reads 96.5% of 5 h")
	// A pi seat records no cache reads: the context shows, the share does not.
	row(RosterAgent{Usage: []Usage{{Window: usage.Window5h, In: 86359, Fresh: 87182, LastContextTokens: 86359, LastAt: now.Add(-2 * time.Minute)}}},
		"context 86.4K at 2m ago")
	// A near-total share truncates to one decimal instead of rounding up:
	// only a genuinely zero fresh count prints 100.0 (field observation
	// 2026-11-25: 99.96% rounded to 100.0 would overstate the share).
	row(RosterAgent{Usage: []Usage{{Window: usage.Window5h, CacheRead: 9996, Fresh: 4, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)}}},
		"context 887.8K at 1h ago · cache reads 99.9% of 5 h")
	// Zero fresh with cache reads is genuinely 100.0.
	row(RosterAgent{Usage: []Usage{{Window: usage.Window5h, CacheRead: 1000, Fresh: 0, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)}}},
		"context 887.8K at 1h ago · cache reads 100.0% of 5 h")
	// No counted response (no mapped session, or a pure output turn): no line.
	row(RosterAgent{}, "")
	row(RosterAgent{Usage: []Usage{{Window: usage.Window5h, LastContextTokens: 0, LastAt: now.Add(-time.Hour)}}}, "")

	// Rendered text: the near-total share truncates to 99.9 on its own
	// indented line after the seat's usage line.
	m := Build(Input{Results: []NamedResult{{Name: "amq", Result: Result{Status: StatusOK, ObservedAt: now, Feeds: []Column{ColumnWaiting}}}}, Config: DefaultConfig(), Now: now})
	m.Roster = []RosterSession{{Session: "s1", Agents: []RosterAgent{{RosterEntry: RosterEntry{Session: "s1", Handle: "w", ValidHandle: true},
		Usage: []Usage{{Window: usage.Window5h, CacheRead: 9996, Fresh: 4, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)}}}}}}
	var out strings.Builder
	if err := WriteText(&out, m); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	for i, l := range lines {
		if strings.Contains(l, "cache reads") && i > 0 && strings.Contains(lines[i-1], "5 h ") {
			if l != "      context 887.8K at 1h ago · cache reads 99.9% of 5 h" {
				t.Errorf("context line = %q, want its own indented line with the truncated 99.9 share", l)
			}
			return
		}
	}
	t.Errorf("text has no context line under the usage line:\n%s", out.String())
}

// The HTML seat renders the context line as its own paragraph inside the
// Usage details, after the spend line — not appended to it.
func TestHTMLSeatContextIsItsOwnParagraph(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	m := Build(Input{Results: []NamedResult{{Name: "amq", Result: Result{Status: StatusOK, ObservedAt: now, Feeds: []Column{ColumnWaiting}}}}, Config: DefaultConfig(), Now: now})
	m.Roster = []RosterSession{{Session: "s1", Agents: []RosterAgent{{RosterEntry: RosterEntry{Session: "s1", Handle: "w", ValidHandle: true},
		Usage: []Usage{{Window: usage.Window5h, Fresh: 1394337, CacheRead: 38630586, LastContextTokens: 887765, LastAt: now.Add(-time.Hour)}}}}}}
	var page strings.Builder
	if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	const spend = "5 h 1.4M fresh, 38.6M cached"
	if !strings.Contains(html, "<p>"+spend+"</p><p>context 887.8K at 1h ago · cache reads 96.5% of 5 h</p>") {
		t.Errorf("the context line is not its own paragraph after the spend line:\n%s", html)
	}
}
