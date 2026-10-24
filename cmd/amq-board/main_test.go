package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	boardusage "github.com/avivsinai/agent-message-queue/internal/board/usage"
)

type noGit struct{}

func (noGit) Run(context.Context, string, []string) ([]byte, error) {
	return nil, errors.New("not a git repository")
}

// The "usage" key of ~/.amq/board.json turns usage off, picks the homes and
// sets the budget. Regression for Codex review of PR #1203 (P2: amq-board
// always read the default homes with the default budget).
func TestBoardReadsUsageSettings(t *testing.T) {
	claude, err := filepath.Abs("../../internal/board/usage/testdata/claude")
	if err != nil {
		t.Fatal(err)
	}
	env := board.Env{Dir: "/work/demo", Now: func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }, Run: noGit{}}
	read := func(t *testing.T, settings string) *board.UsageView {
		t.Helper()
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".amq"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".amq", "board.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := withUsage(board.Model{}, env, home)
		if err != nil {
			t.Fatal(err)
		}
		return m.Usage
	}

	if u := read(t, `{"usage":{"enabled":false}}`); u != nil {
		t.Errorf("usage disabled still read: %+v", u)
	}
	claudeJSON := filepath.ToSlash(claude)
	u := read(t, `{"usage":{"homes":{"claude":["`+claudeJSON+`"],"codex":[],"pi":[]},"budget":{"wall":"5s","bytes":"1B"}}}`)
	if u == nil || len(u.Accounts) != 1 || u.Accounts[0].Harness != boardusage.Claude {
		t.Fatalf("accounts = %+v, want only the configured Claude home", u)
	}
	if !u.Accounts[0].Partial {
		t.Errorf("a 1 byte budget read the whole home: %+v", u.Accounts[0])
	}
}
