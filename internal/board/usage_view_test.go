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
