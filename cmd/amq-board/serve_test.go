package main

import (
	"context"
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
	withUsage(ctx, board.Model{}, env, st)
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
			return withUsage(ctx, board.Model{}, env, cfg.Usage)
		},
		Now:  time.Now,
		Logf: func(string, ...any) {},
	}
	write(`{"usage": {"enabled": false}}`)
	l.Refresh(context.Background(), false)
	if l.Model().Usage != nil {
		t.Fatal("usage read while disabled")
	}
	write(`{"liveness_window": "soon", "usage": {"enabled": true}}`)
	l.Refresh(context.Background(), false)
	if l.Model().Usage != nil {
		t.Fatal("a refused edit turned usage on")
	}
}
