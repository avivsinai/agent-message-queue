package codex

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestCloseIndependentOfBlockedWriter pins the last B14 acceptance clause:
// connection close must not depend on acquiring the blocked writer. A writer
// wedged in writeFrame (peer stopped reading; the frame exceeds every
// intermediate buffer) holds wmu forever — before the fix, close() queued on
// wmu behind it and never returned. net.Pipe is used for the transport
// because a unix-socket peer's kernel buffers can absorb even a 64 MiB
// frame, making the wedge non-deterministic; a net.Pipe write blocks until
// read, always.
func TestCloseIndependentOfBlockedWriter(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix socket bind unavailable in this environment")
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = acceptServerWS(conn) // handshake only; never reads frames
	}()

	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Swap the transport for a never-read in-memory pipe: every Write blocks
	// until read, so the wedge below is deterministic.
	pipeR, pipeW := net.Pipe()
	defer pipeR.Close()
	ws.conn = pipeW

	wedged := make(chan error, 1)
	go func() { wedged <- ws.writeText(make([]byte, 1<<20)) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-wedged:
		t.Fatalf("write did not wedge (returned %v) — test cannot pin the wedge", err)
	default:
	}

	done := make(chan error, 1)
	go func() { done <- ws.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("Close blocked behind wedged writer\n%s", buf[:n])
	}
	// The wedged writer must have been aborted by the close, not left stuck:
	select {
	case err := <-wedged:
		if err == nil {
			t.Fatal("wedged write completed successfully after close")
		}
	case <-time.After(2 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("wedged writer never aborted by close\n%s", buf[:n])
	}
}
