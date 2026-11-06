//go:build unix

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// up.log gets a timestamp per line and stays under its cap by keeping one
// older file (an unbounded 544 KB up.log with no timestamps, bead
// agent-message-queue-9df).
func TestUpLogTimestampsAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := logSink(dir, f)
	l, ok := w.(*upLog)
	if !ok {
		t.Fatalf("logSink(up.log) = %T, want *upLog", w)
	}
	l.max = 400
	for i := range 20 {
		say(w, "link example: state %d", i)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) > 400 || len(old) > 400 {
		t.Fatalf("up.log %d bytes, up.log.1 %d bytes, want each at most 400", len(cur), len(old))
	}
	line := regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ link example: state \d+$`)
	if got := len(line.FindAll(cur, -1)); got == 0 {
		t.Fatalf("up.log has no timestamped lines:\n%s", cur)
	}
	if !regexp.MustCompile(`state 19\n$`).Match(cur) {
		t.Fatalf("up.log does not end with the last line:\n%s", cur)
	}
}
