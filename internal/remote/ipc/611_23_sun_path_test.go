//go:build darwin || linux

package ipc

import (
	"errors"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Test61123SocketPathTooLong verifies that Listen returns ErrSocketPathTooLong
// with a helpful message instead of a bare EINVAL from bind when the computed
// socket path exceeds the Unix sun_path limit.
// Bead agent-message-queue-611.23.
func Test61123SocketPathTooLong(t *testing.T) {
	// Build a state dir whose SocketPath will exceed maxUnixSocketPathLen.
	// SocketPath = stateDir + "/endpoint.sock" (16 bytes for the suffix).
	suffix := "/endpoint.sock"
	need := maxUnixSocketPathLen - len(suffix) + 1
	if need < 1 {
		need = 1
	}
	longDir := strings.Repeat("x", need)

	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store})
	_, err = Listen(longDir, ep)
	if !errors.Is(err, ErrSocketPathTooLong) {
		t.Fatalf("Listen(long dir) err = %v, want ErrSocketPathTooLong", err)
	}
}
