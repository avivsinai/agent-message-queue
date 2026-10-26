package collect_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/usagecache"
	"github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/collect"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// fakeCodex answers `codex --version` and the app-server run with captured
// stdout, and counts app-server runs. Like Codex, the app-server names the
// canonical path of the CODEX_HOME it was given. versionTakes moves the clock
// while the version check runs.
type fakeCodex struct {
	version      string
	stdout       []byte
	versionTakes time.Duration
	mu           sync.Mutex
	clock        time.Time
	runs         int
	perHome      bool // the account id in the reply follows the home
}

func (f *fakeCodex) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeCodex) run(_ context.Context, c edge.Command) edge.CommandReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Equal(c.Args, []string{"--version"}) {
		f.clock = f.clock.Add(f.versionTakes)
		return edge.CommandReply{Stdout: []byte("codex-cli " + f.version + "\n"), ExitCode: usage.Some(0)}
	}
	f.runs++
	stdout := f.stdout
	for _, e := range c.Env {
		if dir, ok := strings.CutPrefix(e, "CODEX_HOME="); ok {
			if canon, err := filepath.EvalSymlinks(dir); err == nil {
				dir = canon
			}
			named, _ := json.Marshal(dir)
			stdout = bytes.ReplaceAll(stdout, []byte(`"/codex-home"`), named)
			if f.perHome {
				stdout = bytes.ReplaceAll(stdout, []byte(`"accountId":"`), []byte(`"accountId":"`+filepath.Base(filepath.Dir(dir))))
			}
		}
	}
	if c.Done == nil || !c.Done(stdout) {
		return edge.CommandReply{Stdout: stdout, Failure: usage.FailTimeout}
	}
	return edge.CommandReply{Stdout: stdout, ExitCode: usage.Some(0)}
}

func setup(t *testing.T, fixture, version string) (*fakeCodex, edge.Effects, collect.Request, string) {
	t.Helper()
	stdout, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "usage")
	store, err := usagecache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "codex-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeCodex{version: version, stdout: stdout, clock: now}
	fx := edge.Effects{Clock: f.now, Command: f.run, Store: store}
	req := collect.Request{
		Codex:    collect.Codex{Bin: "codex", Homes: []string{home}},
		Interval: 5 * time.Minute, Deadline: 3 * time.Second,
		ClientName: "amq-board", ClientVersion: "test",
	}
	return f, fx, req, dir
}

// The null-windows fixtures are real replies of codex 0.163.0 and 0.164.0
// on a plan whose windows are null, with emails and ids scrubbed; the
// windows fixture is built from the protocol schema codex 0.163.0
// generates, with the weekly window in the primary slot.
func TestRunCodexFacts(t *testing.T) {
	for _, tc := range []struct {
		fixture   string
		version   string
		linked    bool // the home is reached through a symlink
		accountID string
		windows   map[string]usage.Window // by slot
		money     []usage.Money
		status    usage.Status
	}{
		{
			fixture:   "app-server-null-windows.jsonl",
			version:   "0.163.0",
			accountID: "00000000-0000-4000-8000-000000000001",
			windows: map[string]usage.Window{
				"primary":   {LimitID: "codex", Slot: "primary", Reported: usage.Null},
				"secondary": {LimitID: "codex", Slot: "secondary", Reported: usage.Null},
			},
			money: []usage.Money{
				{LimitID: "codex", Kind: usage.MoneyBalance, Unit: "credits", Amount: usage.Some("209.7055460214615"), Unlimited: usage.Some(false)},
				{LimitID: "codex", Kind: usage.MoneyCap, Amount: usage.Some("290.2944539785385"), Limit: usage.Some("500.00"),
					RemainingPercent: usage.Some(42.0), ResetsAt: usage.Some(time.Unix(1793491200, 0).UTC())},
			},
			status: usage.StatusPartial,
		},
		{
			fixture:   "app-server-0.164.0.jsonl",
			version:   "0.164.0",
			accountID: "00000000-0000-4000-8000-000000000002",
			windows: map[string]usage.Window{
				"primary":   {LimitID: "codex", Slot: "primary", Reported: usage.Null},
				"secondary": {LimitID: "codex", Slot: "secondary", Reported: usage.Null},
			},
			money: []usage.Money{
				{LimitID: "codex", Kind: usage.MoneyBalance, Unit: "credits", Amount: usage.Some("198.5649837255478"), Unlimited: usage.Some(false)},
				{LimitID: "codex", Kind: usage.MoneyCap, Amount: usage.Some("301.4350162744522"), Limit: usage.Some("500.00"),
					RemainingPercent: usage.Some(40.0), ResetsAt: usage.Some(time.Unix(1793491201, 0).UTC())},
			},
			status: usage.StatusPartial,
		},
		{
			fixture:   "app-server-windows.jsonl",
			version:   "0.163.0",
			accountID: "00000000-0000-4000-8000-000000000002",
			windows: map[string]usage.Window{
				"primary": {LimitID: "codex", Slot: "primary", Reported: usage.Present, Kind: usage.WindowWeekly,
					DurationMinutes: usage.Some[int64](10080), UsedPercent: usage.Some(12.0), ResetsAt: usage.Some(time.Unix(1793491200, 0).UTC())},
				"secondary": {LimitID: "codex", Slot: "secondary", Reported: usage.Present, Kind: usage.Window5h,
					DurationMinutes: usage.Some[int64](300), UsedPercent: usage.Some(37.0), ResetsAt: usage.Some(time.Unix(1792940400, 0).UTC())},
			},
			money: []usage.Money{
				{LimitID: "codex", Kind: usage.MoneyBalance, Unit: "credits", Amount: usage.Some("0"), Unlimited: usage.Some(false)},
			},
			status: usage.StatusFresh,
		},
		{
			// usage slice review finding 1: Codex canonicalizes CODEX_HOME,
			// so a home reached through a symlink read as another home.
			fixture:   "app-server-windows.jsonl",
			version:   "0.163.0",
			linked:    true,
			accountID: "00000000-0000-4000-8000-000000000002",
			windows: map[string]usage.Window{
				"primary": {LimitID: "codex", Slot: "primary", Reported: usage.Present, Kind: usage.WindowWeekly,
					DurationMinutes: usage.Some[int64](10080), UsedPercent: usage.Some(12.0), ResetsAt: usage.Some(time.Unix(1793491200, 0).UTC())},
				"secondary": {LimitID: "codex", Slot: "secondary", Reported: usage.Present, Kind: usage.Window5h,
					DurationMinutes: usage.Some[int64](300), UsedPercent: usage.Some(37.0), ResetsAt: usage.Some(time.Unix(1792940400, 0).UTC())},
			},
			money: []usage.Money{
				{LimitID: "codex", Kind: usage.MoneyBalance, Unit: "credits", Amount: usage.Some("0"), Unlimited: usage.Some(false)},
			},
			status: usage.StatusFresh,
		},
	} {
		name := tc.fixture
		if tc.linked {
			name += "/linked"
		}
		t.Run(name, func(t *testing.T) {
			_, fx, req, _ := setup(t, tc.fixture, tc.version)
			if tc.linked {
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(req.Codex.Homes[0], link); err != nil {
					t.Fatal(err)
				}
				req.Codex.Homes = []string{link}
			}
			home := req.Codex.Homes[0]
			got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
			if len(got.Failures) != 0 {
				t.Fatalf("failures: %+v", got.Failures)
			}
			want := usage.Account{Provider: usage.ProviderCodex, Home: home, Remote: usage.RemoteKey(usage.ProviderCodex, tc.accountID)}
			if len(got.Windows) != len(tc.windows) {
				t.Fatalf("windows: %+v", got.Windows)
			}
			for _, w := range got.Windows {
				if w.Account != want || w.Value != tc.windows[w.Value.Slot] {
					t.Errorf("window %s = %+v %+v", w.Value.Slot, w.Account, w.Value)
				}
			}
			var money []usage.Money
			for _, m := range got.Money {
				money = append(money, m.Value)
			}
			if !slices.Equal(money, tc.money) {
				t.Errorf("money = %+v, want %+v", money, tc.money)
			}
			if a := usage.Assess(got, usage.ProviderCodex, home, now, 15*time.Minute); a.Status != tc.status {
				t.Errorf("status = %s, want %s", a.Status, tc.status)
			}
			if read := collect.Read(context.Background(), fx.Store); len(read.Windows) != len(got.Windows) || len(read.Money) != len(got.Money) {
				t.Errorf("snapshot does not hold the published facts: %+v", read)
			}
		})
	}
}

func TestRunTwoCallersOneChild(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collect.Run(context.Background(), req, collect.Memory{}, fx)
		}()
	}
	wg.Wait()
	got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
	if f.runs != 1 {
		t.Fatalf("app-server ran %d times, want 1", f.runs)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("third caller did not get the first caller's facts: %+v", got)
	}
}

// PR #1235 review: a release nobody ran against passed the version guard as
// verified because it lay inside a range.
func TestRunSkipsUnverifiedCodex(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.165.0")
	home := req.Codex.Homes[0]
	got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
	if f.runs != 0 {
		t.Fatalf("app-server ran %d times for an unverified version", f.runs)
	}
	if len(got.Failures) != 1 || got.Failures[0].Value.Code != usage.FailUnsupportedVersion {
		t.Fatalf("failures = %+v", got.Failures)
	}
	if a := usage.Assess(got, usage.ProviderCodex, home, now, 15*time.Minute); a.Status != usage.StatusUnknown {
		t.Fatalf("status = %s, want unknown", a.Status)
	}
}

// PR #1235 review: the reservation's deadline was set before the version
// check, and the app-server still started with a fresh full deadline after
// that deadline had passed.
func TestRunDoesNotDispatchPastTheReservedDeadline(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	f.versionTakes = req.Deadline + time.Second
	got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
	if f.runs != 0 {
		t.Fatalf("app-server started %d times after the pass deadline", f.runs)
	}
	if len(got.Failures) != 1 || got.Failures[0].Value.Code != usage.FailTimeout {
		t.Fatalf("failures = %+v", got.Failures)
	}
}

// PR #1235 review: a schema-1 journal attempt without its deadline and
// interval held nothing back, so the next run dispatched at once; and an
// attempt that finished before it was reserved (a wall clock set back
// during the run) released its reservation early.
func TestRunHonorsTheJournal(t *testing.T) {
	attempt := func(home, fields string) string {
		key, _ := json.Marshal(home)
		return `{"schema":1,"attempts":[{"id":"a","operation":"codex.app-server.rate-limits",` +
			`"key":{"provider":"codex","home":` + string(key) + `},` + fields + `}]}`
	}
	for _, tc := range []struct {
		name    string
		journal func(home string) string
		failure string
		runs    int
		dropped bool // the attempt is gone from the journal after the pass
		kept    bool // the journal file was set aside, and the next pass finds a clean one
	}{
		{
			// post-release review v0.113.2 finding U1: an entry the reader
			// rejects blocked collection for good; it is dropped with a
			// diagnostic and the pass runs.
			name:    "incomplete attempt",
			journal: func(h string) string { return attempt(h, `"reserved_at":"2026-10-25T11:59:00Z"`) },
			failure: usage.FailJournal,
			runs:    1,
			dropped: true,
		},
		{
			// post-release review v0.113.2 finding U1: a journal file that
			// cannot be parsed blocked collection forever; it is set aside
			// and the pass starts from an empty journal.
			name:    "garbage journal",
			journal: func(string) string { return "not json" },
			failure: usage.FailJournal,
			runs:    1,
			kept:    true,
		},
		{
			name: "deadline before reserved",
			journal: func(h string) string {
				return attempt(h, `"reserved_at":"2026-10-25T11:50:10Z","deadline_at":"2026-10-25T11:50:03Z",`+
					`"interval":300000000000`)
			},
			failure: usage.FailJournal,
			runs:    1,
			dropped: true,
		},
		{
			name: "finished before reserved",
			journal: func(h string) string {
				return attempt(h, `"reserved_at":"2026-10-25T11:50:00Z","deadline_at":"2026-10-25T11:50:03Z",`+
					`"interval":300000000000,"finished_at":{"presence":"present","value":"2026-10-25T10:30:00Z"},`+
					`"failure_code":"unsupported_version"`)
			},
		},
		{
			// usage slice review finding 2: one failing home held back
			// every other home of the provider for its failure hold.
			name: "another home failed",
			journal: func(string) string {
				return attempt("/other-home", `"reserved_at":"2026-10-25T11:49:00Z","deadline_at":"2026-10-25T11:49:03Z",`+
					`"interval":300000000000,"finished_at":{"presence":"present","value":"2026-10-25T11:49:02Z"},`+
					`"failure_code":"home_mismatch"`)
			},
			runs: 1,
		},
		{
			// usage slice review finding 9: an attempt reserved in the
			// future (a wall clock set back since) held dispatch until wall
			// time caught up, and the journal never dropped it.
			name: "reserved in the future",
			journal: func(h string) string {
				return attempt(h, `"reserved_at":"2026-10-25T13:00:00Z","deadline_at":"2026-10-25T13:00:03Z",`+
					`"interval":300000000000`)
			},
			runs:    1,
			dropped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fx, req, dir := setup(t, "app-server-windows.jsonl", "0.163.0")
			if err := os.WriteFile(filepath.Join(dir, "coordination-v1.json"), []byte(tc.journal(req.Codex.Homes[0])), 0o600); err != nil {
				t.Fatal(err)
			}
			got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
			if f.runs != tc.runs {
				t.Fatalf("app-server ran %d times against the journal, want %d", f.runs, tc.runs)
			}
			var codes []string
			for _, x := range got.Failures {
				codes = append(codes, x.Value.Code)
			}
			if want := []string{tc.failure}; tc.failure == "" && len(codes) != 0 || tc.failure != "" && !slices.Equal(codes, want) {
				t.Fatalf("failures = %v, want %q", codes, tc.failure)
			}
			if tc.kept {
				if m, _ := filepath.Glob(filepath.Join(dir, "coordination-v1.json.unreadable-*")); len(m) != 1 {
					t.Fatalf("kept files = %v, want 1", m)
				}
				f.clock = f.clock.Add(req.Interval + time.Second)
				if again, _ := collect.Run(context.Background(), req, collect.Memory{}, fx); len(again.Failures) != 0 {
					t.Fatalf("next pass failures = %+v", again.Failures)
				}
			}
			if tc.dropped {
				j, err := fx.Store.LoadJournal()
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range j.Attempts {
					if a.ID == "a" {
						t.Fatalf("journal kept the dropped attempt: %+v", a)
					}
				}
			}
		})
	}
}

// usage slice review finding 3: each pass that read an identity but no
// readings added one more identity to the snapshot.
func TestRunKeepsOneIdentityPerHome(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	lines := bytes.SplitAfter(f.stdout, []byte("\n"))
	f.stdout = append(bytes.Join(lines[:2], nil), []byte(`{"id":3,"error":{"code":-32000}}`+"\n")...)
	for range 2 {
		collect.Run(context.Background(), req, collect.Memory{}, fx)
		f.clock = f.clock.Add(10 * time.Minute)
	}
	if f.runs != 2 {
		t.Fatalf("app-server ran %d times, want 2", f.runs)
	}
	if got := collect.Read(context.Background(), fx.Store); len(got.Identities) != 1 {
		t.Fatalf("snapshot holds %d identities, want 1: %+v", len(got.Identities), got.Identities)
	}
}

// failingSnapshot is a store whose snapshot cannot be written.
type failingSnapshot struct{ edge.Persistence }

func (failingSnapshot) SaveFacts(usage.Result) error { return errors.New("disk full") }

// usage slice review finding 4: a pass whose snapshot was not written was
// recorded as a success.
func TestRunRecordsAFailedSnapshotWrite(t *testing.T) {
	_, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	fx.Store = failingSnapshot{fx.Store}
	collect.Run(context.Background(), req, collect.Memory{}, fx)
	j, err := fx.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Attempts) != 1 || j.Attempts[0].FailureCode != usage.FailCacheWrite {
		t.Fatalf("journal = %+v, want one attempt that failed with %s", j.Attempts, usage.FailCacheWrite)
	}
}

// clockStepStore moves the clock forward when the lock is taken, as an NTP
// step or a sleep between the pass's first clock reading and its reservation.
type clockStepStore struct {
	edge.Persistence
	f    *fakeCodex
	step time.Duration
}

func (s clockStepStore) TryLock() (func(), bool, error) {
	s.f.mu.Lock()
	s.f.clock = s.f.clock.Add(s.step)
	s.f.mu.Unlock()
	return s.Persistence.TryLock()
}

// post-release review v0.113.2 finding U1: a forward clock step between the
// pass deadline and the reservation stored a deadline before its own
// reservation, which the reader refused for good.
func TestRunKeepsTheJournalReadableAcrossAClockStep(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	fx.Store = clockStepStore{Persistence: fx.Store, f: f, step: 10 * time.Second}
	collect.Run(context.Background(), req, collect.Memory{}, fx)
	j, err := fx.Store.LoadJournal()
	if err != nil || len(j.Attempts) != 1 {
		t.Fatalf("journal = %+v, err = %v", j, err)
	}
	if f.runs != 1 {
		t.Fatalf("app-server ran %d times, want 1", f.runs)
	}
}

// post-release review v0.113.2 finding U3: one Codex home was read per
// interval across every home, so with several homes each reading went stale.
// Homes that proved distinct accounts are paced apart.
func TestRunPacesPerAccount(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	second := filepath.Join(t.TempDir(), "codex-home")
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatal(err)
	}
	req.Codex.Homes = append(req.Codex.Homes, second)
	f.perHome = true
	// An unproven home is paced with the whole provider, so the first two
	// passes are an interval apart; each proves its home's account.
	for range 2 {
		collect.Run(context.Background(), req, collect.Memory{}, fx)
		f.clock = f.clock.Add(req.Interval + time.Second)
	}
	f.runs = 0
	for range 2 {
		collect.Run(context.Background(), req, collect.Memory{}, fx)
		f.clock = f.clock.Add(time.Second)
	}
	if f.runs != 2 {
		t.Fatalf("app-server ran %d times for two homes of distinct accounts, want 2", f.runs)
	}
}

// post-release review v0.113.2 finding U4: a failed account/read was
// dropped, so a read with no identity looked complete.
func TestRunKeepsRateLimitsWhenAccountReadFails(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	lines := bytes.SplitAfter(f.stdout, []byte("\n"))
	lines[1] = []byte(`{"id":2,"error":{"code":-32000}}` + "\n")
	f.stdout = bytes.Join(lines, nil)
	got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
	if len(got.Windows) != 2 || len(got.Failures) != 1 || got.Failures[0].Value.Code != usage.FailRPC ||
		!strings.Contains(got.Failures[0].Value.Detail, "account/read") {
		t.Fatalf("windows %d, failures %+v", len(got.Windows), got.Failures)
	}
	if a := usage.Assess(got, usage.ProviderCodex, req.Codex.Homes[0], now, 15*time.Minute); a.Status != usage.StatusPartial {
		t.Fatalf("status = %s, want partial", a.Status)
	}
}

// post-release review v0.113.2 finding U4 follow-up: a failed account/read
// beside good limits set the attempt's failure code, so pacing backed off to
// 15 minutes and every reading went stale.
func TestRunPartialReadKeepsTheInterval(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.163.0")
	lines := bytes.SplitAfter(f.stdout, []byte("\n"))
	lines[1] = []byte(`{"id":2,"error":{"code":-32000}}` + "\n")
	f.stdout = bytes.Join(lines, nil)
	for range 6 {
		collect.Run(context.Background(), req, collect.Memory{}, fx)
		f.clock = f.clock.Add(req.Interval + time.Second)
	}
	if f.runs != 6 {
		t.Fatalf("app-server ran %d times in 6 intervals, want 6", f.runs)
	}
}
