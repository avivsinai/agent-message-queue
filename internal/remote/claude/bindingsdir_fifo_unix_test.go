//go:build !windows

package claude

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Bead agent-message-queue-7wq, Pro review of #952 r2: a FIFO named
// bindings/y.json must not hang the Stop hook, and counts as presence.
func TestStopHookDoesNotBlockOnBindingsFIFO(t *testing.T) {
	const sid = "session-y"
	home := t.TempDir()
	t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "binding.json"))
	if err := os.Mkdir(filepath.Join(home, "bindings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, "bindings", "y.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bindStopSession(home, sid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		RunStopHookReceiver(home, strings.NewReader(`{"session_id":"session-y","hook_event_name":"Stop"}`), io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the Stop hook blocked on a FIFO in bindings/")
	}
	if _, err := os.Stat(stopMarkerPath(home, sid)); !os.IsNotExist(err) {
		t.Fatal("a FIFO binding entry let a stale sentinel write")
	}
}
