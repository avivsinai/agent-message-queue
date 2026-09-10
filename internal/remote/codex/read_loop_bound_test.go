package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadLoopNotBlockedBySlowCallback pins the B14 reader-callback bound: a
// blocking OnNotification handler must not stall the read pump. While the
// first notification's (slow) handler is still running, subsequent frames —
// here the response to a Call — must still be delivered: the Call completes
// on its context even though the handler is wedged. Before dispatchAsync the
// read loop invoked handlers synchronously and the wedged handler starved
// the pending-call receive.
func TestReadLoopNotBlockedBySlowCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("uses unix socket")
	}
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

	// The stand-in app-server: after answering the request, it pushes two
	// notifications back to back. The client's handler for the first one
	// blocks on hold until released, so a synchronous read loop would never
	// deliver the second — and, more importantly, the response to the Call
	// (frame 2, behind notification frame 1) would never be correlated.
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
		if err := json.Unmarshal(payload, &req); err != nil || req.ID == nil {
			return
		}
		// Frame 1: a notification (its client handler will block).
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"slow"}}}`))
		// Frame 2: the response the Call is waiting for.
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*req.ID) + `,"result":{"turn":{"id":"u1"}}}`))
		// Frame 3: a second notification the wedged handler must not starve.
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"slow","status":"completed"}}}`))
	}()

	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newClient(ws)
	t.Cleanup(func() { _ = c.Close() })

	release := make(chan struct{})
	var started atomic.Bool
	c.OnNotification = func(n Notification) {
		if n.Method == "turn/started" {
			started.Store(true)
			<-release // wedge the handler on the FIRST notification
		}
	}

	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// With dispatchAsync, the response behind the notification still arrives.
	if err := c.Call(ctx, "turn/start", map[string]any{"threadId": "t1", "input": []map[string]string{{"type": "text", "text": "hi"}}}, &result); err != nil {
		t.Fatalf("call blocked behind slow notification handler: %v", err)
	}
	if result.Turn.ID != "u1" {
		t.Fatalf("turn id = %q, want u1", result.Turn.ID)
	}
	if !started.Load() {
		// The notification was dispatched; give it a moment and re-check.
		deadline := time.Now().Add(time.Second)
		for !started.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !started.Load() {
			t.Fatal("first notification never dispatched")
		}
	}
	close(release)
}
