package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveFixture is a Live whose one source is an AMQ roster with alpha last
// seen 45 minutes ago, so a 30m liveness window shows alpha quiet and a 1h
// window shows it live.
func liveFixture(t *testing.T, userPath string, logs *[]string, builds *int) *Live {
	t.Helper()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	amq := Result{
		Status: StatusOK, ObservedAt: now,
		AMQ:    &AMQContext{Sessions: []SessionInfo{{Name: "s1", Path: "/q/s1"}}},
		Roster: []RosterEntry{{Session: "s1", Handle: "alpha", ValidHandle: true, Counted: true, PresenceStatus: "active", PresenceAgeSeconds: 45 * 60}},
	}
	return &Live{
		Load: func() (FileConfig, []string) { return LoadConfig(userPath, "") },
		Build: func(_ context.Context, cfg FileConfig, _ bool) Model {
			*builds++
			return Build(Input{Results: []NamedResult{{Name: "amq", Result: amq}}, Config: cfg.Board, Now: now})
		},
		Now:  func() time.Time { return now },
		Logf: func(format string, args ...any) { *logs = append(*logs, format) },
	}
}

func liveness(m Model) string {
	for _, rs := range m.Roster {
		for _, a := range append(rs.Agents, rs.Quiet...) {
			if a.Handle == "alpha" {
				return a.Liveness
			}
		}
	}
	return "absent"
}

// Design 13 row 7 and 8: serve re-reads board.json on every refresh; a
// valid edit applies without a restart, and a refused edit keeps the last
// good value (not the defaults) and is reported once.
func TestLiveReloadsConfigAndKeepsLastGoodOnRefusedEdit(t *testing.T) {
	userPath := filepath.Join(t.TempDir(), ConfigFile)
	write := func(s string) {
		if err := os.WriteFile(userPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var logs []string
	var builds int
	l := liveFixture(t, userPath, &logs, &builds)
	ctx := context.Background()

	write(`{"liveness_window": "30m"}`)
	l.Refresh(ctx, false)
	if got := liveness(l.Model()); got != "quiet" {
		t.Fatalf("30m window: alpha is %s, want quiet", got)
	}

	write(`{"liveness_window": "1h"}`)
	l.Refresh(ctx, false)
	if got := liveness(l.Model()); got != "live" {
		t.Fatalf("after the edit to 1h: alpha is %s, want live", got)
	}

	write(`{"liveness_window": "1h",`)
	l.Refresh(ctx, false)
	l.Refresh(ctx, false)
	if got := liveness(l.Model()); got != "live" {
		t.Fatalf("after a refused edit: alpha is %s, want live (the last good 1h, not the 30m default)", got)
	}
	refused := 0
	for _, line := range logs {
		if strings.Contains(line, "refused") {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("refused edit reported %d times, want once: %q", refused, logs)
	}
}

// Design 8 route contract (finding 15): every route needs the token; without
// it model.json and refresh are 404 and a refresh is never started.
func TestLiveRoutesNeedTheToken(t *testing.T) {
	const token, host = "0123456789abcdef0123456789abcdef", "127.0.0.1:4100"
	var logs []string
	var builds int
	l := liveFixture(t, filepath.Join(t.TempDir(), ConfigFile), &logs, &builds)
	l.Refresh(context.Background(), false)
	h := l.Handler(token, host)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/model.json", http.StatusNotFound},
		{"/refresh", http.StatusNotFound},
		{"/0123456789abcdef0123456789abcdee/model.json", http.StatusNotFound},
		{"/0123456789abcdef0123456789abcdee/refresh", http.StatusNotFound},
		{"/" + token + "/model.json", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
	if builds != 1 {
		t.Fatalf("a request without the token started a refresh: %d builds, want 1", builds)
	}
}

// Codex review of PR #1215: a refresh a press started ran on
// context.Background, so Ctrl-C neither cancelled its source commands nor
// waited for them. It runs under the view's context, and Wait returns once
// it has ended.
func TestLivePressedRefreshEndsWithTheView(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelled := false
	l := &Live{
		Load: func() (FileConfig, []string) { return DefaultFileConfig(), nil },
		Build: func(c context.Context, _ FileConfig, _ bool) Model {
			close(started)
			select {
			case <-c.Done():
				cancelled = true
			case <-time.After(2 * time.Second):
			}
			return Model{}
		},
		Now:  time.Now,
		Logf: func(string, ...any) {},
		Ctx:  ctx,
	}
	if got := l.Request(); got != "started" {
		t.Fatalf("Request = %q, want started", got)
	}
	<-started
	cancel()
	l.Wait()
	if !cancelled {
		t.Fatal("the pressed refresh did not see the view end")
	}
}
