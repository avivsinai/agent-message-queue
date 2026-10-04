package codex

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the accept key.
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

// acceptServerWS performs the server side of the upgrade on an accepted
// connection, for tests that stand in for the app-server.
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

// writeText sends one text frame with no deadline, for test servers.
func (w *wsConn) writeText(payload []byte) error {
	return w.writeTextCtx(context.Background(), payload)
}
