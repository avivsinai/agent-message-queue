package codex

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression for agent-message-queue-611.22.40 (gate 2026-09-15, union
// 42d923a4): the opClose arm of readText called writeFrame, which acquires
// the writer slot with lockWrite — no ctx, no write deadline. B14b bounded
// only the pong arm. With the writer slot wedged (a large in-flight write
// blocking inside conn.Write), a peer Close frame stopped the read pump:
// readText never returned, readLoop never closed c.closed, Done() never
// fired, and the attachment never reported offline. Fix: the close-frame
// reply is deadline-bounded like the pong (3s); on timeout the stream is
// poisoned and the pump still exits. Same discipline applied to
// Client.Respond, the one remaining unbounded write.
func TestGate1ClosePumpStallsOnWedgedWriter(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// The server accepts, completes the WS handshake, sends a Close frame —
	// and never reads, so the client's writer can wedge.
	serverClosed := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","method":"close","params":{}}`))
		// Send a raw close frame: opcode 0x88, len 0, unmasked (server->client
		// frames are unmasked).
		_, _ = ws.conn.Write([]byte{0x88, 0x00})
		close(serverClosed)
		// Deliberately never read or write again.
	}()
	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newClient(ws, Handlers{})
	t.Cleanup(func() { _ = c.Close() })

	// Wedge the writer deterministically (verifier round-1 recut): hold the
	// writer slot in the test itself. The 512KB-socket-fill approach depended
	// on kernel auto-tuned socket buffers — the pre-fix gate passed 3/4 runs,
	// a flake. Holding ws.wmu is the exact precondition readText's close
	// reply contends on: with the fix reverted, writeCloseReply blocks in
	// lockWrite unboundedly and the pump deadlocks (witnessed deterministically);
	// with the fix, the 3s deadline fires and the pump exits.
	ws.wmu <- struct{}{} // hold the writer slot for the whole test

	<-serverClosed // the Close frame is on the wire

	// The pump must exit despite the wedged writer slot: readText's
	// close-frame reply must be bounded.
	select {
	case <-c.Done():
		// Pump exited — readErr should be io.EOF (clean close).
		c.mu.Lock()
		readErr := c.readErr
		c.mu.Unlock()
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, errStreamPoisoned) && !errors.Is(readErr, net.ErrClosed) {
			t.Fatalf("pump exited with unexpected error %v", readErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read pump did not exit within 5s of peer Close with wedged writer (611.22.40)")
	}
}

// Verifier round-1 TEST FIX (agent-message-queue-611.22.40): the Respond
// half shipped untested — reverting writeTextBounded -> writeText at
// rpc.go:310 left the package green. This test goes red on that revert:
// with the writer slot held, Respond must return an error within the
// bounded budget instead of blocking forever on the unbounded slot wait.
func TestGate1RespondBoundedBehindHeldSlot(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = acceptServerWS(conn)
		// deliberately never read or write
	}()
	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newClient(ws, Handlers{})
	t.Cleanup(func() { _ = c.Close() })

	ws.wmu <- struct{}{} // hold the writer slot for the whole test

	done := make(chan error, 1)
	go func() {
		done <- c.Respond(json.RawMessage(`"srv-1"`), map[string]any{"ok": true})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Respond returned nil despite the held writer slot; the bounded write should have timed out (611.22.40)")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Respond blocked past the 3s bound on a held slot — writeTextBounded not in the path (611.22.40)")
	}
}
