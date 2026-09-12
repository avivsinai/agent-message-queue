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

// TestHandlersAreInstalledBeforeTheReadPumpStarts reproduces
// agent-message-queue-611.22.25: newClient started readLoop and reqWorker,
// both of which read c.OnNotification / c.OnServerRequest, while the caller
// assigned those exported fields AFTER Dial returned. That is a data race on
// the fields, and a window in which a frame arriving first found a nil
// handler and was silently dropped. Handlers are now constructor arguments,
// so a frame delivered immediately — before the caller could have assigned
// anything — still reaches its handler.
func TestHandlersAreInstalledBeforeTheReadPumpStarts(t *testing.T) {
	sock, srv := startFakeAppServer(t)

	notes := make(chan Notification, 4)
	reqs := make(chan ServerRequest, 4)
	c, err := Dial(sock, Handlers{
		OnNotification:  func(n Notification) { notes <- n },
		OnServerRequest: func(r ServerRequest) { reqs <- r },
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Barrier: one real call proves the server has accepted the connection
	// (srv.ws is assigned by the accept goroutine, and the test harness reads
	// it without synchronisation). The point of this test is what happens
	// BEFORE any handler could have been assigned post-Dial, and no
	// assignment happens anywhere in it — the handlers came from Dial.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Call(ctx, "initialize", map[string]any{}, nil); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	<-srv.calls

	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	select {
	case n := <-notes:
		if n.Method != "turn/started" {
			t.Fatalf("notification = %q, want turn/started", n.Method)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notification delivered before any post-Dial assignment was dropped")
	}
}
