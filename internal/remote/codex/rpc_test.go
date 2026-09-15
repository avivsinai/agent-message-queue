package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClientRoundTripOverUnixWebSocket is the transport happy path: a stand-in
// app-server accepts the WebSocket upgrade over a unix socket, answers one
// request, pushes one notification and one server request, and receives our
// response to it.
func TestClientRoundTripOverUnixWebSocket(t *testing.T) {
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

	gotResponse := make(chan json.RawMessage, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		payload, err := ws.readText()
		if err != nil {
			return
		}
		var req rpcMessage
		if err := json.Unmarshal(payload, &req); err != nil {
			return
		}
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"u1"}}}`))
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*req.ID) + `,"result":{"turn":{"id":"u1"}}}`))
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":"srv-1","method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"u1","itemId":"i1"}}`))
		resp, err := ws.readText()
		if err != nil {
			return
		}
		gotResponse <- resp
	}()

	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	h := &swappableHandlers{}
	c := newClient(ws, h.handlers())
	attachTestHandlers(c, h)
	t.Cleanup(func() { _ = c.Close() })
	notes := make(chan Notification, 4)
	handlersOf(c).setNote(func(n Notification) { notes <- n })
	handlersOf(c).setReq(func(r ServerRequest) {
		_ = c.Respond(r.ID, map[string]string{"decision": "decline"})
	})

	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Call(ctx, "turn/start", map[string]any{"threadId": "t1", "input": []map[string]string{{"type": "text", "text": "hi"}}}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Turn.ID != "u1" {
		t.Fatalf("unexpected result %+v", result)
	}
	select {
	case n := <-notes:
		if n.Method != "turn/started" {
			t.Fatalf("unexpected notification %s", n.Method)
		}
	case <-ctx.Done():
		t.Fatal("no notification")
	}
	select {
	case resp := <-gotResponse:
		var msg rpcMessage
		if err := json.Unmarshal(resp, &msg); err != nil || msg.ID == nil || string(*msg.ID) != `"srv-1"` {
			t.Fatalf("server request answer not correlated: %s", resp)
		}
	case <-ctx.Done():
		t.Fatal("server request never answered")
	}
}
