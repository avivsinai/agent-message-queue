package usage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeLineJSON(version, msgID string) string {
	return `{"type":"assistant","timestamp":"2026-10-22T11:00:00.000Z","sessionId":"aaaaaaaa-0000-4000-8000-000000000001","cwd":"/work/demo","version":"` + version + `","message":{"id":"` + msgID + `","usage":{"input_tokens":2,"cache_creation_input_tokens":275,"cache_read_input_tokens":800,"output_tokens":650}}}`
}

func demoEligible(cwd string) bool { return cwd == "/work/demo" }

// An unreadable session file and a skipped over-long line both leave the
// totals incomplete, so the account must say "at least", never show a
// complete figure. Regression for Codex review of PR #1203 (P2: unreadable
// files and skipped lines counted as complete reads).
func TestIncompleteReadsKeepTheAccountPartial(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-work-demo")
	long := `{"type":"assistant","usage":"` + strings.Repeat("x", maxLine) + `"}`
	writeFile(t, filepath.Join(dir, "aaaaaaaa-0000-4000-8000-000000000001.jsonl"),
		claudeLineJSON("2.1.300", "msg_a"), long, claudeLineJSON("2.1.300", "msg_b"))
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		locked := filepath.Join(dir, "bbbbbbbb-0000-4000-8000-000000000002.jsonl")
		writeFile(t, locked, claudeLineJSON("2.1.300", "msg_c"))
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
	}
	rep := Read(Config{Homes: map[string][]string{Claude: {home}}}, testNow, demoEligible, nil)
	if a := rep.Accounts[0]; !a.Partial() {
		t.Fatalf("account with an unread file and a skipped line is complete: %+v", a)
	}
}

// Records of a format version the readers were not checked against are not
// counted, tokens or limits: the spend is unknown, not a number. Regression
// for Codex review of PR #1203 (P2: no version guard before counting).
func TestUnverifiedVersionsAreNotCounted(t *testing.T) {
	claude, codex, pi := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(claude, "projects", "-work-demo", "aaaaaaaa-0000-4000-8000-000000000001.jsonl"),
		claudeLineJSON("3.0.0", "msg_a"))
	writeFile(t, filepath.Join(codex, "sessions", "2026", "10", "22", "rollout-2026-10-22T06-00-00-cccccccc-0000-4000-8000-000000000003.jsonl"),
		`{"timestamp":"2026-10-22T06:00:00.000Z","type":"session_meta","payload":{"id":"cccccccc-0000-4000-8000-000000000003","cwd":"/work/demo","cli_version":"1.0.0","source":"cli"}}`,
		`{"timestamp":"2026-10-22T11:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":100,"total_tokens":1100}}}}`)
	writeFile(t, filepath.Join(pi, "agent", "sessions", "--work-demo--", "2026-10-22T10-00-00-000Z_eeeeeeee-0000-4000-8000-000000000005.jsonl"),
		`{"type":"session","version":4,"id":"eeeeeeee-0000-4000-8000-000000000005","timestamp":"2026-10-22T10:00:00.000Z","cwd":"/work/demo"}`,
		`{"type":"message","timestamp":"2026-10-22T10:00:05.000Z","message":{"role":"assistant","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}`)
	cfg := Config{Homes: map[string][]string{Claude: {claude}, Codex: {codex}, Pi: {pi}}}
	rep := Read(cfg, testNow, demoEligible, nil)
	if len(rep.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(rep.Accounts))
	}
	for _, a := range rep.Accounts {
		for _, s := range a.Sessions {
			if c, _ := s.Totals(testNow.Add(-span)); c.Fresh() != 0 || c.CacheRead != 0 {
				t.Errorf("%s counted an unverified record: %+v", a.Key, c)
			}
		}
		if !a.Partial() {
			t.Errorf("%s totals look complete despite an unverified file", a.Key)
		}
		var reasons []string
		for _, l := range a.Limits {
			reasons = append(reasons, l.Reason)
		}
		if !strings.Contains(strings.Join(reasons, "; "), "not verified") {
			t.Errorf("%s limits = %v, want the unverified version named", a.Key, reasons)
		}
	}
}

// Listing session files and reading one long line both spend the run's
// budget: a spent wall budget lists nothing more, and a byte budget stops
// inside a line instead of after it. Regression for Codex review of
// PR #1203 (P2: discovery outside the budget; a long line read whole).
func TestReaderStaysInsideItsBudget(t *testing.T) {
	t.Run("listing", func(t *testing.T) {
		home := t.TempDir()
		for _, slug := range []string{"-work-demo", "-work-other"} {
			writeFile(t, filepath.Join(home, "projects", slug, "aaaaaaaa-0000-4000-8000-000000000001.jsonl"), claudeLineJSON("2.1.300", "msg_a"))
		}
		rep := Read(Config{Homes: map[string][]string{Claude: {home}}, BudgetWall: time.Nanosecond}, testNow, demoEligible, nil)
		if a := rep.Accounts[0]; a.Files != 0 || !a.Partial() {
			t.Fatalf("spent budget still listed %d files (partial %v)", a.Files, a.Partial())
		}
	})
	t.Run("long line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "long.jsonl")
		writeFile(t, path, `{"usage":"`+strings.Repeat("x", 3<<20)+`"}`)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		const allowance = 64 << 10
		b := &budget{deadline: time.Now().Add(time.Hour), bytes: allowance, clock: time.Now}
		skipped := 0
		done := scan(f, 0, claudeUsageKeys, b, &skipped, func([]byte) bool { return true })
		if spent := allowance - b.bytes; done || spent > allowance+1<<20 {
			t.Fatalf("read %d bytes on a %d byte budget (done %v), want a stop within one chunk", spent, allowance, done)
		}
	})
}
