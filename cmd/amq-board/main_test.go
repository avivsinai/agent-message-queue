package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
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
