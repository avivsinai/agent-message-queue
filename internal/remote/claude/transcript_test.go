package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression (session mirror review, round 1): after the tail of a line
// longer than one chunk was skipped, the next line, which ran past the end
// of that read, was taken for another over-long line and skipped too.
func TestReadTranscriptKeepsTheLineAfterAnOverlongOne(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	next := `{"type":"user","uuid":"u2","message":{"content":"` + strings.Repeat("z", 3<<20) + `"}}`
	data := strings.Repeat("x", 6<<20) + "\n" + next + "\n" + `{"type":"user","uuid":"u3"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []int
	off, skipping := int64(0), false
	for range 10 {
		c, err := ReadTranscript(path, off, skipping)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range c.Lines {
			got = append(got, len(l))
		}
		off, skipping = c.Next, c.Skipping
	}
	if len(got) != 2 || got[0] != len(next) {
		t.Fatalf("line lengths = %v; want the %d-byte line and the last line", got, len(next))
	}
}
