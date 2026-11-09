//go:build darwin || linux

package fsq

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An idle sweep reads an unchanged file once, and a write is read again
// (agent-message-queue-udg: serve and the Buzz agent decoded every registry
// and binding file on each sweep).
func TestVersionCacheReadsAnUnchangedFileOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(FileVersionSettle + 100*time.Millisecond)
	}
	c, reads := NewVersionCache[string](4), 0
	load := func() string {
		v, err := c.Load(path, func() (string, error) {
			reads++
			raw, err := os.ReadFile(path)
			return string(raw), err
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	write("a")
	first, second := load(), load()
	if first != "a" || second != "a" || reads != 1 {
		t.Fatalf("an unchanged file was read %d times", reads)
	}
	write("b")
	if got := load(); got != "b" || reads != 2 {
		t.Fatalf("after a write: %q, %d reads", got, reads)
	}
}
