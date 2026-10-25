package usage

import (
	"fmt"
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

var demoRoots = []string{"/work/demo"}

// Each way a read can miss data leaves the totals incomplete, so the
// account must say "at least", never show a complete figure. Regression for
// Codex review of PR #1203 (P2: unreadable files, unsearchable directories
// and skipped lines counted as complete reads).
func TestIncompleteReadsKeepTheAccountPartial(t *testing.T) {
	session := func(home string) string {
		return filepath.Join(home, "projects", "-work-demo", "aaaaaaaa-0000-4000-8000-000000000001.jsonl")
	}
	unixOnly := func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs Unix permissions that bind the test user")
		}
	}
	partial := func(t *testing.T, home string) {
		t.Helper()
		rep := Read(Config{Homes: map[string][]string{Claude: {home}}}, testNow, demoRoots, nil)
		if a := rep.Accounts[0]; !a.Partial() {
			t.Fatalf("account is complete: %+v", a)
		}
	}
	t.Run("line over the limit", func(t *testing.T) {
		home := t.TempDir()
		long := `{"type":"assistant","usage":"` + strings.Repeat("x", maxLine) + `"}`
		writeFile(t, session(home), claudeLineJSON("2.1.300", "msg_a"), long, claudeLineJSON("2.1.300", "msg_b"))
		partial(t, home)
	})
	t.Run("unreadable file", func(t *testing.T) {
		unixOnly(t)
		home := t.TempDir()
		writeFile(t, session(home), claudeLineJSON("2.1.300", "msg_a"))
		if err := os.Chmod(session(home), 0); err != nil {
			t.Fatal(err)
		}
		partial(t, home)
	})
	t.Run("unsearchable directory", func(t *testing.T) {
		unixOnly(t)
		home := t.TempDir()
		writeFile(t, session(home), claudeLineJSON("2.1.300", "msg_a"))
		dir := filepath.Dir(session(home))
		if err := os.Chmod(dir, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		partial(t, home)
	})
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
	rep := Read(cfg, testNow, demoRoots, nil)
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
		// The wall budget is spent as soon as the run starts.
		home := t.TempDir()
		for _, slug := range []string{"-work-demo", "-work-other"} {
			writeFile(t, filepath.Join(home, "projects", slug, "aaaaaaaa-0000-4000-8000-000000000001.jsonl"), claudeLineJSON("2.1.300", "msg_a"))
		}
		t0, started := time.Now(), false
		clock := func() time.Time {
			if !started {
				started = true
				return t0
			}
			return t0.Add(time.Hour)
		}
		rep := read(Config{Homes: map[string][]string{Claude: {home}}}, testNow, demoRoots, nil, clock)
		if a := rep.Accounts[0]; a.Files != 0 || !a.Partial() {
			t.Fatalf("spent budget still listed %d files (partial %v)", a.Files, a.Partial())
		}
	})
	t.Run("large directory", func(t *testing.T) {
		// The clock runs out after the second check: on the listing of a
		// directory with three batches of entries, that is before the third
		// batch, so no session below it is listed.
		root := t.TempDir()
		for i := 0; i < 3*listBatch; i++ {
			writeFile(t, filepath.Join(root, fmt.Sprintf("d%04d", i), "s.jsonl"), "{}")
		}
		t0 := time.Now()
		calls := 0
		b := &budget{deadline: t0.Add(time.Second), bytes: 1 << 30, clock: func() time.Time {
			calls++
			if calls > 2 {
				return t0.Add(time.Hour)
			}
			return t0
		}}
		files, cut := listPattern(root, []string{"*", "s.jsonl"}, testNow.Add(-span), func(string) bool { return false }, b)
		if len(files) != 0 || !cut {
			t.Fatalf("listed %d sessions (cut %v) after the budget ran out inside a directory", len(files), cut)
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

// A one-shot board must count this project's recent sessions even when the
// home holds many files of other projects: listing them cannot spend the
// budget reading needs. Regression for the live board of 2026-10-25, whose
// Claude account read "partial: 0 of 560 files; session files not all
// listed".
func TestListingCannotStarveRecentProjectSessions(t *testing.T) {
	home := t.TempDir()
	at := func(path string, mtime time.Time, lines ...string) {
		writeFile(t, path, lines...)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	other := `{"type":"assistant","timestamp":"2026-10-22T11:50:00.000Z","sessionId":"bbbbbbbb-0000-4000-8000-000000000002","cwd":"/work/other","version":"2.1.300","message":{"id":"msg_o","usage":{"input_tokens":1,"output_tokens":1}}}`
	for d := 0; d < 6; d++ {
		for i := 0; i < 50; i++ {
			at(filepath.Join(home, "projects", fmt.Sprintf("-work-other-%d", d), fmt.Sprintf("old-%02d.jsonl", i)), testNow.Add(-10*24*time.Hour), other)
		}
	}
	for i := 0; i < 20; i++ {
		at(filepath.Join(home, "projects", "-work-other-0", fmt.Sprintf("busy-%02d.jsonl", i)), testNow.Add(-10*time.Minute), other)
	}
	for i := 0; i < 3; i++ {
		at(filepath.Join(home, "projects", "-work-demo", fmt.Sprintf("recent-%d.jsonl", i)), testNow.Add(-time.Hour), claudeLineJSON("2.1.300", fmt.Sprintf("msg_%d", i)))
	}
	// Each budget check advances the clock 10 ms, so the 1 s budget holds
	// 100 checks: fewer than one listing of every file needs.
	t0 := time.Now()
	ticks := 0
	clock := func() time.Time {
		ticks++
		return t0.Add(time.Duration(ticks) * 10 * time.Millisecond)
	}
	cfg := Config{Homes: map[string][]string{Claude: {home}}, BudgetWall: time.Second}
	a := read(cfg, testNow, []string{"/work/demo"}, nil, clock).Accounts[0]
	var fresh int64
	for _, s := range a.Sessions {
		c, _ := s.Totals(testNow.Add(-span))
		fresh += c.Fresh()
	}
	if want := int64(3 * (2 + 275 + 650)); fresh != want || a.Read < 3 {
		t.Fatalf("project sessions counted %d fresh tokens from %d of %d files (%s), want %d", fresh, a.Read, a.Files, a.Detail, want)
	}
	if !a.Partial() {
		t.Fatalf("account is complete although the budget stopped it: %+v", a)
	}
}

// Each spend record counts once, at its final value. Regressions for the
// design review of usage-library-design-r2 (2026-10-25): Claude kept the
// first streaming row of a message although a later row carries the final
// output; a Codex total that went down counted again in full; a pi fork
// copies its parent's entries, which counted twice.
func TestEachSpendRecordCountsOnce(t *testing.T) {
	const sid = "aaaaaaaa-0000-4000-8000-000000000001"
	claudeRow := func(out int) string {
		return `{"type":"assistant","timestamp":"2026-10-22T11:00:00.000Z","sessionId":"` + sid + `","cwd":"/work/demo","version":"2.1.300","message":{"id":"msg_a","usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":` + fmt.Sprint(out) + `}}}`
	}
	codexTotal := func(at string, total int) string {
		return `{"timestamp":"2026-10-22T` + at + `.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":` + fmt.Sprint(total) + `,"cached_input_tokens":0,"output_tokens":0,"total_tokens":` + fmt.Sprint(total) + `}}}}`
	}
	piEntry := `{"type":"message","id":"a1b2c3d4","parentId":null,"timestamp":"2026-10-22T10:00:05.000Z","message":{"role":"assistant","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}`
	piSession := func(id string) string {
		return `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-10-22T10:00:00.000Z","cwd":"/work/demo"}`
	}
	for _, tc := range []struct {
		name    string
		harness string
		files   map[string][]string
		want    int64
	}{
		{"claude streaming rows", Claude, map[string][]string{
			"projects/-work-demo/" + sid + ".jsonl": {claudeRow(1), claudeRow(650)},
		}, 2 + 650},
		{"codex total goes down", Codex, map[string][]string{
			"sessions/2026/10/22/rollout-2026-10-22T11-00-00-cccccccc-0000-4000-8000-000000000003.jsonl": {
				`{"timestamp":"2026-10-22T11:00:00.000Z","type":"session_meta","payload":{"id":"cccccccc-0000-4000-8000-000000000003","cwd":"/work/demo","cli_version":"0.163.0","source":"cli"}}`,
				codexTotal("11:00:01", 1000), codexTotal("11:00:02", 1500), codexTotal("11:00:03", 1200),
			},
		}, 1500},
		{"pi fork copies parent entries", Pi, map[string][]string{
			"agent/sessions/--work-demo--/2026-10-22T10-00-00-000Z_eeeeeeee-0000-4000-8000-000000000005.jsonl": {
				piSession("eeeeeeee-0000-4000-8000-000000000005"), piEntry,
			},
			"agent/sessions/--work-demo--/2026-10-22T10-30-00-000Z_ffffffff-0000-4000-8000-000000000006.jsonl": {
				piSession("ffffffff-0000-4000-8000-000000000006"), piEntry,
			},
		}, 150},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for path, lines := range tc.files {
				writeFile(t, filepath.Join(home, filepath.FromSlash(path)), lines...)
			}
			a := Read(Config{Homes: map[string][]string{tc.harness: {home}}}, testNow, demoRoots, nil).Accounts[0]
			var fresh int64
			for _, s := range a.Sessions {
				c, _ := s.Totals(testNow.Add(-span))
				fresh += c.Fresh()
			}
			if fresh != tc.want {
				t.Fatalf("counted %d fresh tokens, want %d", fresh, tc.want)
			}
		})
	}
}

// "windows not reported by this plan" is a claim about the plan, so it needs
// a rate limit report that carried no windows; with none read, the windows
// are unknown. Regression for the design review of usage-library-design-r2
// (2026-10-25): the text showed with no Codex record read at all.
func TestCodexPlanWindowsNeedARateLimitReport(t *testing.T) {
	a := Read(Config{Homes: map[string][]string{Codex: {t.TempDir()}}}, testNow, demoRoots, nil).Accounts[0]
	for _, l := range a.Limits {
		if strings.Contains(l.Reason, "not reported by this plan") {
			t.Fatalf("limit %+v claims the plan's windows with no rate limit report read", l)
		}
	}
}
