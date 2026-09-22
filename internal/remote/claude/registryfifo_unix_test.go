//go:build !windows

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// testRegistryFIFO probes a FIFO at the session-registry leaf: refused
// without opening (an open would block until a writer appears — the exact
// r2 P1-1 freeze under the endpoint mutex). Unix-only: mkfifo is not in
// the Windows syscall package.
func testRegistryFIFO(t *testing.T, home, regPath string) {
	t.Helper()
	if err := os.Remove(regPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(regPath, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := readSessionRegistry(home, 77); err == nil {
		t.Fatal("accepted a FIFO at the session-registry path")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("refusal does not name the file-type rule: %v", err)
	}
}

// codex #855 r1 items 8 and receiver boundary: a FIFO at the peer-key leaf
// or at the Stop-marker leaf is refused without blocking.
func TestFIFOLeavesNeverBlock(t *testing.T) {
	home := tempHome(t, 9, &sessionRegistry{Pid: 9, SessionID: "s9", Kind: "interactive"})
	sock := "/tmp/cc-socks/9.sock"
	key, err := peerKeyFile(home, 9, sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(key, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := stopMarkerPath(home, "s9")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readPeerToken(home, 9, sock)
		RunStopHookReceiver(home, strings.NewReader(`{"session_id":"s9"}`), os.Stderr)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readPeerToken accepted a FIFO key file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a FIFO leaf blocked the key read or the receiver")
	}
}

// codex #858 r2: a FIFO at ~/.claude/sessions blocked discovery waiting for
// a writer. It is now refused at open, without blocking.
func TestDiscoverRefusesFIFOSessionsDir(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(claudeSessionsDir(home), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := discoverer{home: home}.Discover(context.Background(), registry.DiscoverRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("discovery accepted a FIFO as the sessions directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery blocked on a FIFO sessions directory")
	}
}
