package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

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
		Build: func(_ context.Context, cfg board.FileConfig, _ bool) board.Model {
			return withUsage(board.Model{}, env, cfg.Usage)
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
