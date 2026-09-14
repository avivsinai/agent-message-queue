package codex

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

// partialWriteConn fails the first Write after reporting a SHORT count, the
// way net.Conn does when a write deadline expires mid-frame. Every later
// Write is recorded so the test can prove none happened.
type partialWriteConn struct {
	net.Conn
	writes [][]byte
	closed bool
}

func (c *partialWriteConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), p...))
	// Short count + error: exactly the io.Writer contract for a partial write.
	return len(p) / 2, errors.New("i/o timeout")
}
func (c *partialWriteConn) Close() error                       { c.closed = true; return nil }
func (c *partialWriteConn) SetWriteDeadline(t time.Time) error { return nil }

// TestB38PartialFrameWritePoisonsTheConnection pins bead
// agent-message-queue-611.22.38: a frame write that fails has already put a
// TRUNCATED frame on the wire, so the connection is desynchronized and must
// not be reused. Without this, the next frame is consumed by the peer as the
// tail of the previous frame's payload and every later request is corrupt.
func TestB38PartialFrameWritePoisonsTheConnection(t *testing.T) {
	fc := &partialWriteConn{}
	w := &wsConn{conn: fc, br: bufio.NewReader(bytes.NewReader(nil)), wmu: make(chan struct{}, 1)}

	if err := w.writeText([]byte(`{"one":1}`)); err == nil {
		t.Fatal("first write should report the partial-write error")
	}
	if len(fc.writes) != 1 {
		t.Fatalf("first write reached the conn %d times, want 1", len(fc.writes))
	}
	if !fc.closed {
		t.Fatal("a partial frame write must close the connection — the stream is unrecoverable (B38)")
	}

	// The whole point: the SECOND write must not reach a desynchronized peer.
	err := w.writeText([]byte(`{"two":2}`))
	if !errors.Is(err, errStreamPoisoned) {
		t.Fatalf("second write err = %v, want errStreamPoisoned (B38)", err)
	}
	if len(fc.writes) != 1 {
		t.Fatalf("second write reached the conn; writes=%d, want it to fail fast at 1 (B38)", len(fc.writes))
	}
}
