package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	boardusage "github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// Codex review of PR #1215: usage ran `git worktree list` on
// context.Background, so Ctrl-C could not cancel it and serve waited for
// that refresh forever. A cancelled refresh ends the git child promptly.
func TestUsageGitCallEndsWithTheRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	env := board.Env{Dir: t.TempDir(), Now: time.Now, Run: execRunner{}}
	st := boardusage.Settings{Enabled: true, Config: boardusage.DefaultConfig(t.TempDir())}
	start := time.Now()
	withUsage(ctx, board.Model{}, env, st, nil)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("usage returned %v after its refresh was cancelled; want within 2s", elapsed)
	}
}

// Codex review of PR #1215: usage reread ~/.amq/board.json on every refresh
// outside the last-good reload, so an edit serve refused still turned usage
// on. The usage settings travel in the accepted configuration.
func TestServeRefusedEditKeepsUsageSettings(t *testing.T) {
	home := t.TempDir()
	userPath := filepath.Join(home, ".amq", board.ConfigFile)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(userPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	env := board.Env{Dir: dir, Now: time.Now, Run: noGit{}}
	l := &board.Live{
		Load: func() (board.FileConfig, []string) { return loadConfigFiles(dir, home) },
		Build: func(ctx context.Context, cfg board.FileConfig, _ bool) board.Model {
			return withUsage(ctx, board.Model{}, env, cfg.Usage, nil)
		},
		Now:  time.Now,
		Logf: func(string, ...any) {},
	}
	usageOn := func() bool {
		host := "127.0.0.1:1"
		rec := httptest.NewRecorder()
		l.Handler("tok", host).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/tok/model.json", nil))
		var m board.Model
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m.Usage != nil
	}
	write(`{"usage": {"enabled": false}}`)
	l.Refresh(context.Background(), false)
	if usageOn() {
		t.Fatal("usage read while disabled")
	}
	write(`{"liveness_window": "soon", "usage": {"enabled": true}}`)
	l.Refresh(context.Background(), false)
	if usageOn() {
		t.Fatal("a refused edit turned usage on")
	}
}

// A running serve publishes ~/.amq/board/serve.json, private to the user,
// with what a local client needs; its shutdown removes it.
func TestServeFileIsWrittenPrivateAndRemovedOnShutdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	want := serveRecord{URL: "http://127.0.0.1:4242", Token: "abc123", PID: os.Getpid(), StartedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}

	if err := writeServeFile(want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".amq", "board", "serve.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "token", "pid", "started_at"} {
		if _, ok := fields[k]; !ok {
			t.Fatalf("serve.json lacks %q: %s", k, data)
		}
	}
	var got serveRecord
	if err := json.Unmarshal(data, &got); err != nil || got != want {
		t.Fatalf("serve.json = %+v (%v), want %+v", got, err, want)
	}

	removeServeFile(want.PID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve.json still there after shutdown: %v", err)
	}
}

// Stopping the serve that serve.json names leaves the record of another
// running serve in place (audit 2026-11-10).
func TestStoppingOneServeKeepsAnotherDiscoverable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	first := serveRecord{URL: "http://127.0.0.1:1", Token: "a", PID: os.Getpid(), StartedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	second := serveRecord{URL: "http://127.0.0.1:2", Token: "b", PID: os.Getpid() + 1, StartedAt: first.StartedAt.Add(time.Minute)}
	for _, r := range []serveRecord{first, second} {
		if err := writeServeFile(r); err != nil {
			t.Fatal(err)
		}
	}
	removeServeFile(second.PID)
	got, ok := readServeRecord(filepath.Join(home, ".amq", "board", "serve.json"))
	if !ok || got != first {
		t.Fatalf("serve.json = %+v (%v), want %+v", got, ok, first)
	}
}
