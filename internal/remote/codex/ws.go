// Package codex attaches to a running Codex CLI session through the shared
// local app-server daemon. The daemon's unix socket is a WebSocket control
// socket carrying one JSON-RPC message per text frame; this file is the
// minimal RFC 6455 client that transport needs, with no external dependency.
package codex

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the accept key.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// MaxFrameBytes bounds one inbound frame; app-server replies are JSON
// documents that stay far below it.
const MaxFrameBytes = 16 * 1024 * 1024

const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// wsConn is one client WebSocket over an already-dialed connection.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
	// closed is set once close() has torn the connection down. A writer
	// wedged inside writeFrame (peer stopped reading) holds wmu forever;
	// close must not queue behind it (Pro B14: connection close independent
	// of the blocked writer), so close sets this flag and closes the
	// underlying conn first — the blocked Write fails immediately, the
	// wedged writer releases wmu, and close proceeds without it.
	closed atomic.Bool
}

// dialUnixWS connects to a unix socket and performs the WebSocket upgrade.
func dialUnixWS(path string, timeout time.Duration) (*wsConn, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	ws := &wsConn{conn: conn, br: bufio.NewReaderSize(conn, 64*1024)}
	if err := ws.handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ws, nil
}

func (w *wsConn) handshake() error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_ = w.conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = w.conn.SetDeadline(time.Time{}) }()
	if _, err := io.WriteString(w.conn, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(w.br, nil)
	if err != nil {
		return fmt.Errorf("websocket handshake: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("websocket handshake: status %s", resp.Status)
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // protocol-mandated
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return errors.New("websocket handshake: accept key mismatch")
	}
	return nil
}

// writeTextCtx sends one masked text frame bounded by ctx: the write
// deadline is installed, the frame written, and the deadline cleared UNDER
// the writer mutex, so a concurrent writer's deadline can never be
// clobbered by another caller's (Pro B14 recut #5 — the conn-wide
// SetWriteDeadline is not per-call state).
func (w *wsConn) writeTextCtx(ctx context.Context, payload []byte) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return w.writeFrame(opText, payload)
	}
	// Own the writer mutex FIRST, then install the deadline: this writer
	// owns the deadline for the duration of its frame write.
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed.Load() {
		return errors.New("websocket closed")
	}
	_ = w.conn.SetWriteDeadline(dl)
	defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()
	return w.writeFrameLocked(opText, payload)
}

// writeText sends one masked text frame.
func (w *wsConn) writeText(payload []byte) error {
	return w.writeFrame(opText, payload)
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.writeFrameLocked(opcode, payload)
}

// writeFrameLocked writes the frame; the caller owns wmu (and any deadline
// it installed).
func (w *wsConn) writeFrameLocked(opcode byte, payload []byte) error {
	if w.closed.Load() {
		return errors.New("websocket closed")
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(append(header, 0x80|127), ext[:]...)
	}
	header = append(header, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.conn.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

// readText returns the next text frame payload, answering pings and
// returning io.EOF on close.
func (w *wsConn) readText() ([]byte, error) {
	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opText:
			return payload, nil
		case opPing:
			if err := w.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opClose:
			_ = w.writeFrame(opClose, nil)
			return nil, io.EOF
		case opPong:
		default:
			// Binary or continuation frames are not part of this protocol.
			return nil, fmt.Errorf("unexpected websocket opcode %#x", opcode)
		}
	}
}

func (w *wsConn) readFrame() (byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	fin := hdr[0]&0x80 != 0
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > MaxFrameBytes {
		return 0, nil, fmt.Errorf("websocket frame of %d bytes exceeds %d", length, MaxFrameBytes)
	}
	if !fin {
		return 0, nil, errors.New("fragmented websocket frames are not supported")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// close tears the connection down without waiting for any wedged writer:
// it closes the underlying conn FIRST (aborting a blocked Write and
// releasing wmu), then best-effort sends the close frame. Taking wmu around
// the close frame would deadlock behind the very writer we are aborting.
func (w *wsConn) close() error {
	w.closed.Store(true)
	_ = w.conn.Close()
	// The conn is closed; this write fails fast if wmu is contended, and
	// otherwise delivers the close frame for a graceful peer handshake.
	done := make(chan struct{})
	go func() {
		_ = w.writeFrame(opClose, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		// Close frame could not be delivered (writer still holds wmu or the
		// conn is gone): the conn is already closed, which is what matters.
	}
	return nil
}

// acceptServerWS performs the server side of the upgrade on an accepted
// connection. It exists for tests that stand in for the app-server.
func acceptServerWS(conn net.Conn) (*wsConn, error) {
	br := bufio.NewReaderSize(conn, 64*1024)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || key == "" {
		return nil, errors.New("not a websocket upgrade")
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // protocol-mandated
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := io.WriteString(conn, resp); err != nil {
		return nil, err
	}
	return &wsConn{conn: conn, br: br}, nil
}
