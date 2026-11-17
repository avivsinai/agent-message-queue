package turn

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
)

// Regression: #1023, {"turn_timout":"4s"} was accepted as an empty document, so
// a typo silently reset a working turn timeout to the startup value. An
// unknown key is refused and the last good value stays.
func TestSettingsTypoKeepsTheLastGoodValue(t *testing.T) {
	timings := NewTimings("amq-acp", Timing{Timeout: 10 * time.Minute, Poll: 100 * time.Millisecond, Heartbeat: 15 * time.Second})
	t.Setenv("HOME", canonicalTempDir(t))
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	dir, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, settingsFile)
	for _, body := range []string{`{"turn_timeout":"30m"}`, `{"turn_timout":"4s"}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := timings.Load().Timeout; got != 30*time.Minute {
			t.Fatalf("after %s turn timeout=%v; want 30m", body, got)
		}
	}
}

// canonicalTempDir is t.TempDir with symlinks resolved: macOS temp dirs live
// under the /var symlink.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
