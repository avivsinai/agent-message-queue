package collect_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/usagecache"
	"github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/collect"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

const home = "/codex-home"

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// fakeCodex answers `codex --version` and the app-server run with captured
// stdout, and counts app-server runs. versionTakes moves the clock while the
// version check runs.
type fakeCodex struct {
	version      string
	stdout       []byte
	versionTakes time.Duration
	mu           sync.Mutex
	clock        time.Time
	runs         int
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
	if c.Done == nil || !c.Done(f.stdout) {
		return edge.CommandReply{Stdout: f.stdout, Failure: usage.FailTimeout}
	}
	return edge.CommandReply{Stdout: f.stdout, ExitCode: usage.Some(0)}
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
	f := &fakeCodex{version: version, stdout: stdout, clock: now}
	fx := edge.Effects{Clock: f.now, Command: f.run, Store: store}
	req := collect.Request{
		Codex:    collect.Codex{Bin: "codex", Homes: []string{home}},
		Interval: 5 * time.Minute, Deadline: 3 * time.Second,
		ClientName: "amq-board", ClientVersion: "test",
	}
	return f, fx, req, dir
}

// The first fixture is a real reply of codex 0.163.0 on a plan whose
// windows are null, with its email and ids scrubbed; the second is built
// from the protocol schema codex 0.163.0 generates, with the weekly window
// in the primary slot.
func TestRunCodexFacts(t *testing.T) {
	for _, tc := range []struct {
		fixture   string
		accountID string
		windows   map[string]usage.Window // by slot
		money     []usage.Money
		status    usage.Status
	}{
		{
			fixture:   "app-server-null-windows.jsonl",
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
			fixture:   "app-server-windows.jsonl",
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
		t.Run(tc.fixture, func(t *testing.T) {
			_, fx, req, _ := setup(t, tc.fixture, "0.163.0")
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

// PR #1235 review: 0.164.0, which nobody ran against, passed the version
// guard as verified.
func TestRunSkipsUnverifiedCodex(t *testing.T) {
	f, fx, req, _ := setup(t, "app-server-windows.jsonl", "0.164.0")
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
// interval held nothing back, so the next run dispatched at once.
func TestRunRefusesAnIncompleteJournal(t *testing.T) {
	f, fx, req, dir := setup(t, "app-server-windows.jsonl", "0.163.0")
	journal := `{"schema":1,"attempts":[{"id":"a","operation":"codex.app-server.rate-limits",` +
		`"key":{"provider":"codex","home":"/codex-home"},"reserved_at":"2026-10-25T11:59:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "coordination-v1.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := collect.Run(context.Background(), req, collect.Memory{}, fx)
	if f.runs != 0 {
		t.Fatalf("app-server ran %d times past an incomplete journal", f.runs)
	}
	if len(got.Failures) != 1 || got.Failures[0].Value.Code != usage.FailJournal {
		t.Fatalf("failures = %+v", got.Failures)
	}
}
