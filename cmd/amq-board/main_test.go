package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	boardusage "github.com/avivsinai/agent-message-queue/internal/board/usage"
)

const floodEnv = "AMQ_BOARD_TEST_FLOOD"

// TestMain doubles as a command that floods stdout, so the runner is tested
// against a real child process.
func TestMain(m *testing.M) {
	if os.Getenv(floodEnv) == "1" {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 2<<20))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Codex review of PR #1200: provider stdout was buffered in full before the
// 1 MiB check, so a noisy provider could exhaust memory.
func TestProviderOutputStopsAtLimit(t *testing.T) {
	t.Setenv(floodEnv, "1")
	out, err := execRunner{}.RunLimited(context.Background(), t.TempDir(), []string{os.Args[0]}, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "output over 1048576 bytes") || out != nil {
		t.Fatalf("RunLimited = %d bytes, %v; want an output-limit error and no output", len(out), err)
	}
}

// Codex review of PR #1200: stdout was read until EOF before Wait, so a
// provider whose child kept the pipe open outlived its deadline (a 50 ms
// context with sh launching sleep 2 returned after 2.07 s).
func TestCancelledProviderDoesNotWaitForItsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execRunner{}.RunLimited(ctx, t.TempDir(), []string{"sh", "-c", "sleep 2; echo late"}, 1<<20)
	if elapsed := time.Since(start); err == nil || elapsed > 500*time.Millisecond {
		t.Fatalf("RunLimited returned after %v with %v; want an error within 500ms", elapsed, err)
	}
}

type noGit struct{}

func (noGit) Run(context.Context, string, []string) ([]byte, error) {
	return nil, errors.New("not a git repository")
}

func (noGit) RunLimited(context.Context, string, []string, int) ([]byte, error) {
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
		cfg, warnings := loadConfigFiles(t.TempDir(), home)
		if len(warnings) > 0 {
			t.Fatal(warnings)
		}
		return withUsage(context.Background(), board.Model{}, env, cfg.Usage).Usage
	}

	if u := read(t, `{"usage":{"enabled":false}}`); u != nil {
		t.Errorf("usage disabled still read: %+v", u)
	}
	// A listed home is read in addition to the default home.
	claudeJSON := filepath.ToSlash(claude)
	u := read(t, `{"usage":{"homes":{"claude":["`+claudeJSON+`"]},"budget":{"wall":"5s","bytes":"1B"}}}`)
	if u == nil {
		t.Fatal("usage not read")
	}
	var keys []string
	partial := false
	for _, a := range u.Accounts {
		if a.Harness == boardusage.Claude {
			keys = append(keys, a.Key)
			partial = partial || (strings.Contains(a.Key, "testdata") && a.Partial)
		}
	}
	if len(keys) != 2 || !strings.HasSuffix(keys[0], ".claude") || !strings.Contains(keys[1], "testdata") {
		t.Fatalf("claude accounts = %v, want the default home and the listed one", keys)
	}
	if !partial {
		t.Errorf("a 1 byte budget read the whole listed home: %+v", u.Accounts)
	}
}
