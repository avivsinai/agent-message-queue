package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// toolServer is a linked server that offers one tool. A call to "slow"
// answers pending and finishes on call_get; every other call answers ok with
// a result of the requested size.
type toolServer struct {
	t          *testing.T
	srv        *httptest.Server
	frameBytes int
	mu         sync.Mutex
	calls      map[string]map[string]any // call_id -> final reply
	busyOnce   map[string]bool           // call_id -> the next call_get answers busy
}

func newToolServer(t *testing.T) *toolServer {
	ts := &toolServer{t: t, frameBytes: linkio.MaxFrameBytes, calls: map[string]map[string]any{}, busyOnce: map[string]bool{}}
	ts.srv = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *toolServer) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(linkio.MaxFrameBytes)
	ctx := r.Context()
	send := func(f map[string]any) { b, _ := json.Marshal(f); _ = ws.Write(ctx, websocket.MessageText, b) }
	read := func() map[string]any {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return nil
		}
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		return f
	}
	send(map[string]any{"schema": linkio.SchemaFrame, "id": "c1", "body": map[string]any{
		"schema": linkio.SchemaChallenge, "server_id": "srv_example", "nonce": "bm9uY2Utbm9uY2Utbm9uY2Utbm9uY2Utbm9uY2UtMzI"}})
	hello := read()
	send(map[string]any{"schema": linkio.SchemaFrame, "re": hello["id"], "gen": 1, "body": map[string]any{
		"schema": linkio.SchemaWelcome, "user": "example.user", "connection_generation": 1,
		"limits": map[string]any{"frame_bytes": ts.frameBytes, "tasks_in_flight": 4, "tool_calls_in_flight": 8}}})
	for {
		f := read()
		if f == nil {
			return
		}
		body := f["body"].(map[string]any)
		var reply any
		switch body["schema"] {
		case "amq.remote.link.tools/1":
			reply = map[string]any{"tools": []any{map[string]any{"name": "get_issue", "description": "Read one issue.", "write": false,
				"input_schema": map[string]any{"type": "object"}}}}
		case "amq.remote.link.call/1":
			id := body["call_id"].(string)
			args := body["arguments"].(map[string]any)
			final := map[string]any{"call_id": id, "status": "ok", "result": map[string]any{"text": strings.Repeat("x", int(num(args["size"])))}}
			ts.mu.Lock()
			ts.calls[id] = final
			ts.mu.Unlock()
			reply = final
			switch body["tool"] {
			case "slow":
				reply = map[string]any{"call_id": id, "status": "pending"}
			case "busy_once":
				ts.mu.Lock()
				ts.busyOnce[id] = true
				ts.mu.Unlock()
				reply = map[string]any{"call_id": id, "status": "pending"}
			case "write":
				reply = map[string]any{"call_id": id, "status": "pending_approval", "review_url": "https://app.example.test/calls/" + id}
			case "denied":
				reply = map[string]any{"call_id": id, "status": "error", "error": map[string]any{"code": "not_allowed", "message": "denied is not in this link's tool profile"}}
			case "drop":
				_ = ws.Close(websocket.StatusGoingAway, "test drop")
				return
			}
		case "amq.remote.link.call_get/1":
			id := body["call_id"].(string)
			ts.mu.Lock()
			reply = ts.calls[id]
			if ts.busyOnce[id] {
				delete(ts.busyOnce, id)
				reply = map[string]any{"error": map[string]any{"code": "busy", "retry_after_ms": 10}}
			}
			ts.mu.Unlock()
		}
		send(map[string]any{"schema": linkio.SchemaFrame, "re": f["id"], "gen": 1, "body": reply})
	}
}

func num(v any) float64 { n, _ := v.(float64); return n }

// linkEndpoint runs the IPC face of a root whose one link is connected to ts.
func linkEndpoint(t *testing.T, ts *toolServer) string {
	t.Helper()
	root, err := os.MkdirTemp("", "arl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	key, err := linkio.MintDeviceKey(stateDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	c, err := linkio.New(linkio.Config{Name: "example", Key: key, StoreID: "st_test",
		Pin: linkio.Pin{URL: "ws" + strings.TrimPrefix(ts.srv.URL, "http"), ServerID: "srv_example"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.Run(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); c.Status().State != "online"; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("link never online")
		}
	}
	ls := &linkSet{running: map[string]*linkRun{"example": {carrier: c, link: manifest.Link{Name: "example"}}}}
	srv, err := ipc.Listen(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetLinkHandler(ls.request)
	go func() { _ = srv.Serve(ctx) }()
	return root
}

// `link call` prints the tool's result, a 3 MiB result passes through IPC,
// --wait reads a pending call to its final state, and `link call resume KEY`
// returns the same call's receipt.
func TestLinkCallThroughTheEndpoint(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	out, code, err := linkCall([]string{"--root", root, "get_issue", "--args", `{"size": 3145728}`, "--idempotency-key", "k_big"}, io.Discard, &jsonProbe{})
	r, _ := out.(toolCallReply)
	if err != nil || code != 0 || r.Status != "ok" || len(r.Result) < 3<<20 {
		t.Fatalf("call = %d, %v, status %q, %d result bytes; want ok with 3 MiB", code, err, r.Status, len(r.Result))
	}
	out, _, err = linkCall([]string{"--root", root, "slow", "--args", `{"size": 2}`, "--idempotency-key", "k_slow", "--wait"}, io.Discard, &jsonProbe{})
	if r, _ := out.(toolCallReply); err != nil || r.Status != "ok" {
		t.Fatalf("--wait = %+v, %v; want the final ok", out, err)
	}
	out, _, err = linkCall([]string{"--root", root, "resume", "k_slow"}, io.Discard, &jsonProbe{})
	if r, _ := out.(toolCallReply); err != nil || r.Status != "ok" || r.CallID != "c_k_slow" {
		t.Fatalf("resume = %+v, %v; want the stored receipt of c_k_slow", out, err)
	}
}

// The MCP face lists the linked server's tools and calls one over stdio.
func TestMCPFaceListsAndCalls(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_issue","arguments":{"size":3}}}`,
	}, "\n") + "\n"
	var out strings.Builder
	if code, err := mcpServe([]string{"--root", root}, strings.NewReader(in), &out, io.Discard); err != nil || code != 0 {
		t.Fatalf("mcp = %d, %v", code, err)
	}
	byID := map[float64]map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		id, _ := m["id"].(float64)
		byID[id] = m
	}
	if len(byID) != 3 {
		t.Fatalf("got %d replies, want 3 (the notification has none): %s", len(byID), out.String())
	}
	tools := byID[2]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_issue" {
		t.Fatalf("tools/list = %v", byID[2])
	}
	call := byID[3]["result"].(map[string]any)
	text := call["content"].([]any)[0].(map[string]any)["text"].(string)
	if call["isError"] != false || !strings.Contains(text, `"xxx"`) {
		t.Fatalf("tools/call = %v", call)
	}
}

// A call over the link's frame limit is refused before it is sent, and the
// link stays up: one large argument never takes the link down (#1028 B2).
func TestLinkCallOverTheFrameLimitIsRefused(t *testing.T) {
	ts := newToolServer(t)
	ts.frameBytes = 1 << 20
	root := linkEndpoint(t, ts)
	_, _, err := linkCall([]string{"--root", root, "get_issue", "--args", `{"blob": "` + strings.Repeat("x", 2<<20) + `"}`}, io.Discard, &jsonProbe{})
	if protocol.RefusalCode(err) != protocol.CodeInvalid {
		t.Fatalf("err = %v, want invalid (too large)", err)
	}
	out, _, err := linkCall([]string{"--root", root, "get_issue", "--args", `{"size": 2}`}, io.Discard, &jsonProbe{})
	if r, _ := out.(toolCallReply); err != nil || r.Status != "ok" {
		t.Fatalf("the next call = %+v, %v; want ok on the same link", out, err)
	}
}

// busy is "try again later", never a final answer: --wait reads past it.
func TestLinkCallWaitReadsPastBusy(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	out, code, err := linkCall([]string{"--root", root, "busy_once", "--args", `{"size": 1}`, "--wait"}, io.Discard, &jsonProbe{})
	if r, _ := out.(toolCallReply); err != nil || code != 0 || r.Status != "ok" {
		t.Fatalf("--wait = %+v, %d, %v; want ok after the busy", out, code, err)
	}
}

// A write that waits for the owner returns at once over MCP: not an error,
// with where to decide and how to resume.
func TestMCPReturnsPendingApprovalAtOnce(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{}}}` + "\n"
	var out strings.Builder
	if _, err := mcpServe([]string{"--root", root}, strings.NewReader(in), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.String()), &m); err != nil || m.Result.IsError ||
		!strings.Contains(m.Result.Content[0].Text, "https://app.example.test/calls/") || !strings.Contains(m.Result.Content[0].Text, "resume") {
		t.Fatalf("tools/call = %s; want the review URL and the resume line, not an error", out.String())
	}
}

// A connection that drops after a call was sent answers at once that the
// answer will not come, instead of waiting for the deadline (#1028 S4).
func TestLinkCallDropAnswersAtOnce(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	start := time.Now()
	_, _, err := linkCall([]string{"--root", root, "drop", "--args", `{}`}, io.Discard, &jsonProbe{})
	if err == nil || !strings.Contains(err.Error(), "dropped") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s; want 'dropped' at once", err, time.Since(start))
	}
}

// Regression (local e2e N5, ruling pp): a call that ended with status error
// (not_allowed) exits non-zero, through the real command dispatch.
func TestLinkCallErrorStatusExitsNonZero(t *testing.T) {
	root := linkEndpoint(t, newToolServer(t))
	var out, errOut strings.Builder
	code := run([]string{"link", "call", "--root", root, "denied", "--args", "{}"}, strings.NewReader(""), &out, &errOut)
	if code == 0 {
		t.Fatalf("exit 0 for a not_allowed call; stdout %s stderr %s", out.String(), errOut.String())
	}
}
