package codex

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// fakeAppServer answers the app-server methods the attachment uses with the
// shapes recorded from the live probe and the generated schema, and lets the
// test push notifications.
type fakeAppServer struct {
	ws    *wsConn
	calls chan rpcMessage
}

func startFakeAppServer(t *testing.T) (string, *fakeAppServer) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	srv := &fakeAppServer{calls: make(chan rpcMessage, 16)}
	ready := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		srv.ws = ws
		close(ready)
		for {
			payload, err := ws.readText()
			if err != nil {
				return
			}
			var msg rpcMessage
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			srv.calls <- msg
			if msg.ID == nil || msg.Method == "" {
				continue
			}
			switch msg.Method {
			case "initialize":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"userAgent":"fake"}}`))
			case "thread/resume":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"thread":{"id":"t1","cwd":"/work","status":{"type":"idle"}}}}`))
			case "turn/start":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`))
			case "turn/interrupt":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{}}`))
			default:
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32601,"message":"unexpected ` + msg.Method + `"}}`))
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-ready:
		default:
		}
	})
	return sock, srv
}

func (s *fakeAppServer) notify(t *testing.T, method, params string) {
	if err := s.ws.writeText([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + params + `}`)); err != nil {
		t.Fatalf("notify %s: %v", method, err)
	}
}

// TestSubmitBindsTurnAndCompletes is the adapter happy path: submit starts a
// turn with our request id as clientUserMessageId, the user-message item
// echoes it back, the agent message carries the text, and turn/completed
// yields a completed run with that text.
func TestSubmitBindsTurnAndCompletes(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	// drain initialize + thread/resume
	<-srv.calls
	<-srv.calls

	events := make(chan core.NativeEvent, 8)
	att.Subscribe(func(ev core.NativeEvent) { events <- ev })

	s := att.Inspect()
	if s.Harness != "codex" || s.Status != "idle" || s.Project != "/work" || !s.Capabilities.Submit || s.Capabilities.ApproveTool {
		t.Fatalf("unexpected session: %+v", s)
	}

	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111501"}
	adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
	if err != nil || !adm.Admitted || adm.RunID != "turn:u1" {
		t.Fatalf("submit: %+v %v", adm, err)
	}
	call := <-srv.calls
	var params map[string]any
	_ = json.Unmarshal(call.Params, &params)
	if call.Method != "turn/start" || params["clientUserMessageId"] != key.RequestID {
		t.Fatalf("unexpected native call: %s %v", call.Method, params)
	}

	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+key.RequestID+`","content":[]}}`)
	srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","completedAtMs":1,"item":{"type":"agentMessage","id":"i2","text":"PONG"}}`)
	// A second submit while the turn is active is refused as busy, never
	// joined to the running turn.
	adm2, _ := att.Submit(core.BoundRequest{Key: requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111502"}, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "again"}})
	if adm2.Admitted || adm2.Code != protocol.CodeBusy {
		t.Fatalf("busy submit not refused: %+v", adm2)
	}
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)

	select {
	case ev := <-events:
		if ev.Type != core.EventRunCompleted || ev.Key != key || ev.Result == nil || ev.Result.Text != "PONG" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no completion event")
	}
	ev, err := att.Lookup(key, s.Epoch)
	if err != nil || !ev.Known || ev.State != protocol.StateCompleted || ev.Result.Text != "PONG" {
		t.Fatalf("lookup: %+v %v", ev, err)
	}
	if att.Inspect().Status != "idle" {
		t.Fatal("thread not idle after completion")
	}
}
