package main

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
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
