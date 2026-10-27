package board

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// liveFixture is a Live whose one source is an AMQ roster with alpha last
// seen 45 minutes ago, so a 30m liveness window shows alpha quiet and a 1h
// window shows it recent.
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
			model := Build(Input{Results: []NamedResult{{Name: "amq", Result: amq}}, Config: cfg.Board, Now: now, Dir: "/work/proj"})
			model.Generation = 1
			model.PublishedAt = now.Add(-time.Minute)
			return model
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
	if got := liveness(l.Model()); got != "recent" {
		t.Fatalf("after the edit to 1h: alpha is %s, want recent", got)
	}

	write(`{"liveness_window": "1h",`)
	l.Refresh(ctx, false)
	l.Refresh(ctx, false)
	if got := liveness(l.Model()); got != "recent" {
		t.Fatalf("after a refused edit: alpha is %s, want recent (the last good 1h, not the 30m default)", got)
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
	// Suggestion provider pins use this same user-only last-good boundary.
	write(`{"suggestions":{"endpoint":"http://127.0.0.1:11434","model":"qwen3.5:4b","digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`)
	accepted := l.ReloadConfig().Suggestions
	if accepted.Endpoint != "http://127.0.0.1:11434" {
		t.Fatal("valid suggestion pins not applied")
	}
	write(`{"suggestions":{"endpoint":"http://remote.example:11434","model":"qwen3.5:4b","digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`)
	if l.ReloadConfig().Suggestions != accepted {
		t.Fatal("refused provider edit replaced last-good pins")
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
		{"/pane.json", http.StatusNotFound},
		{"/0123456789abcdef0123456789abcdee/pane.json", http.StatusNotFound},
		{"/refresh", http.StatusNotFound},
		{"/0123456789abcdef0123456789abcdee/model.json", http.StatusNotFound},
		{"/0123456789abcdef0123456789abcdee/refresh", http.StatusNotFound},
		{"/" + token + "/model.json", http.StatusOK},
		{"/" + token + "/pane.json", http.StatusOK},
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
	// Native-pane acceptance found the full export above 4 MiB. The cached
	// route and CLI encoder share a thin DTO and never include retained evidence.
	want, err := PaneJSON(l.Model())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/"+token+"/pane.json", nil))
	if !bytes.Equal(rec.Body.Bytes(), want) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pane route did not serve the shared cached projection")
	}
	model := Model{Generation: 7, GeneratedAt: time.Now(), PublishedAt: time.Now().Add(-time.Hour), Dir: "/work/" + strings.Repeat("long-project/", 30), Project: "project", Columns: []ColumnView{{ID: ColumnWaiting}, {ID: ColumnInFlight, Cards: make([]Card, 2)}, {ID: ColumnRequests, Cards: make([]Card, 3)}}, Unresolved: []Message{{Body: strings.Repeat("retained evidence ", 300000)}}, Historical: []Card{{ID: "historical"}}}
	const exact = "amq thread --id 'a b'"
	for i := 0; i < 101; i++ {
		model.Columns[0].Cards = append(model.Columns[0].Cards, Card{ID: "task-a", Title: strings.Repeat("title", 50), Reason: Reason{Text: strings.Repeat("reason", 50)}, Actions: []Action{{Purpose: ActionOwnerMutation, Command: "amq-board cancel --task task-a"}, {Purpose: ActionContext, Command: exact}}})
	}
	model.Columns[0].Cards[1].Actions = []Action{{Purpose: ActionContext, Command: "read\nmutate"}, {Purpose: ActionContext, Command: exact}}
	model.Columns[0].Cards[2].Actions = []Action{{Purpose: ActionContext, Command: strings.Repeat("a", 4097)}}
	for i := 0; i < 34; i++ {
		model.Sources = append(model.Sources, SourceView{Name: strings.Repeat("source", 20), Status: StatusOK})
	}
	full, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) <= PaneJSONLimit {
		t.Fatal("fixture did not reproduce the oversized full export")
	}
	data, err := PaneJSON(model)
	if err != nil {
		t.Fatal(err)
	}
	var pane PaneModel
	if err = json.Unmarshal(data, &pane); err != nil {
		t.Fatal(err)
	}
	if len(data) >= PaneJSONLimit || bytes.Contains(data, []byte("retained evidence")) || pane.CurrentTotal != 106 || pane.WaitingTotal != 101 || len(pane.Waiting) != 100 || pane.WaitingOmitted != 1 || len(pane.Sources) != 32 || pane.SourcesOmitted != 2 || pane.Dir != model.Dir || !pane.PublishedAt.Equal(model.PublishedAt) {
		t.Fatal("pane lost accurate counts, identity, freshness or display bounds")
	}
	if pane.Waiting[0].Command != exact || pane.Waiting[1].Command != "" || pane.Waiting[2].Command != "" || utf8.RuneCountInString(pane.Waiting[0].Title) > 160 || utf8.RuneCountInString(pane.Waiting[0].Reason) > 160 {
		t.Fatal("pane offered an owner mutation, changed a command or exceeded display bounds")
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
