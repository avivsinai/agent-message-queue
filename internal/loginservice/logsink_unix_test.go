//go:build unix

package loginservice

import (
	"fmt"

	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A watched log gets a timestamp per line and stays under its cap by keeping
// one older file (an unbounded 544 KB up.log with no timestamps, bead
// agent-message-queue-9df).
func TestLogSinkTimestampsAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := LogSink(path, f)
	l, ok := w.(*logWriter)
	if !ok {
		t.Fatalf("LogSink(up.log) = %T, want *logWriter", w)
	}
	l.max = 400
	for i := range 20 {
		_, _ = fmt.Fprintf(w, "link example: state %d\n", i)
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

// Review of the disk bounds: a service manager may open the log without
// O_APPEND; the sink must still lock, timestamp and append at the end.
func TestLogSinkWithoutAppendFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := LogSink(path, f)
	if _, ok := w.(*logWriter); !ok {
		t.Fatalf("LogSink(non-append up.log) = %T, want *logWriter", w)
	}
	_, _ = fmt.Fprintln(w, "one")
	_, _ = fmt.Fprintln(w, "two")
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ one\n\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ two\n$`).Match(cur) {
		t.Fatalf("up.log = %q, want two timestamped lines", cur)
	}
}

// Review finding, 2026-11-26: a log whose path was given to another file
// refused every write once it crossed its cap. The writer keeps the lines
// in the file it holds; it does not drop them for the sake of the bound.
func TestLogSinkKeepsWritingWhenItsPathNamesAnotherFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := LogSink(path, f)
	l, ok := w.(*logWriter)
	if !ok {
		t.Fatalf("LogSink(up.log) = %T, want *logWriter", w)
	}
	l.max = 100
	moved := filepath.Join(dir, "moved.log")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if _, err := fmt.Fprintf(w, "link example: state %d\n", i); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	held, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`state 0\n`).Match(held) || !regexp.MustCompile(`state 9\n$`).Match(held) {
		t.Fatalf("the held log lost lines:\n%s", held)
	}
}
