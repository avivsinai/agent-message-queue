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
// wmu is a cap-1 channel semaphore (B14b/B6): acquisition is a select on
// send vs ctx.Done(), so WAITING for the writer is context-bounded — the
// old sync.Mutex acquisition was unbounded, and ctx only bounded the write
// after it. Release is a receive. Zero extra goroutines, no lost unlocks.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  chan struct{}
}

func newWSConn(conn net.Conn, br *bufio.Reader) *wsConn {
	return &wsConn{conn: conn, br: br, wmu: make(chan struct{}, 1)}
}

// writePong sends a pong frame with the writer slot acquired under a
// deadline: both the WAIT for the slot and the write are bounded, so the
// read pump can never stall on a wedged socket (B14b recut secondary).
func (w *wsConn) writePong(payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.lockWriteCtx(ctx); err != nil {
		return err
	}
	defer w.unlockWrite()
	dl, _ := ctx.Deadline()
	_ = w.conn.SetWriteDeadline(dl)
	defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()
	return w.writeFrameBody(opPong, payload)
}

// lockWriteCtx acquires the writer slot or gives up on ctx.
func (w *wsConn) lockWriteCtx(ctx context.Context) error {
	select {
	case w.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lockWrite acquires the writer slot without a context (unbounded wait,
// used only by pings/close-frames which must always land).
func (w *wsConn) lockWrite() {
	w.wmu <- struct{}{}
}

// unlockWrite releases the writer slot.
func (w *wsConn) unlockWrite() { <-w.wmu }

// dialUnixWS connects to a unix socket and performs the WebSocket upgrade.
func dialUnixWS(path string, timeout time.Duration) (*wsConn, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	ws := newWSConn(conn, bufio.NewReaderSize(conn, 64*1024))
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

// writeText sends one masked text frame.
func (w *wsConn) writeText(payload []byte) error {
	return w.writeFrame(opText, payload)
}

// writeTextCtx sends one masked text frame bounded by ctx in BOTH phases:
// acquiring the writer slot (select on ctx.Done) and the write itself
// (deadline installed+cleared under the slot, so concurrent writers cannot
// clobber each other's deadline). This is Pro B14 recut #5's
// deadline-under-mutex discipline with context-bounded acquisition added
// (B14b/B6).
func (w *wsConn) writeTextCtx(ctx context.Context, payload []byte) error {
	if err := w.lockWriteCtx(ctx); err != nil {
		return err
	}
	defer w.unlockWrite()
	dl, ok := ctx.Deadline()
	if ok {
		_ = w.conn.SetWriteDeadline(dl)
		defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()
	}
	return w.writeFrameBody(opText, payload)
}

// writeFrame sends one frame, acquiring the writer slot without a context
// (pings, pongs, close frames, and Respond must always land).
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.lockWrite()
	defer w.unlockWrite()
	return w.writeFrameBody(opcode, payload)
}
func (w *wsConn) writeFrameBody(opcode byte, payload []byte) error {
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
			// The pong reply runs INSIDE the read pump, so it is
			// deadline-bounded: an unbounded write on a wedged socket would
			// stall the pump (B14b recut secondary finding).
			if err := w.writePong(payload); err != nil {
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

func (w *wsConn) close() error {
	// Try to send a close frame without blocking forever behind a wedged
	// writer: the conn.Close below aborts any in-flight write anyway, and
	// close must never hang (a blocked close-frame send wedges teardown).
	select {
	case w.wmu <- struct{}{}:
		_ = w.writeFrameBody(opClose, nil)
		<-w.wmu
	default:
	}
	return w.conn.Close()
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
	return newWSConn(conn, br), nil
}
