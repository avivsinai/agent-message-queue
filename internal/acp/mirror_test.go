package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/remote/activity"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// codexFake is the fake runtime with the Codex notification seam, so the
// test drives native activity the way the Codex adapter reports it.
type codexFake struct {
	*fake.Runtime
	mu       sync.Mutex
	notify   func(codex.Notification)
	observed chan struct{}
}

func (c *codexFake) ObserveNotifications(fn func(codex.Notification)) func() {
	c.mu.Lock()
	c.notify = fn
	c.mu.Unlock()
	c.observed <- struct{}{}
	return func() {
		c.mu.Lock()
		c.notify = nil
		c.mu.Unlock()
	}
}

func (c *codexFake) send(method, params string) {
	c.mu.Lock()
	fn := c.notify
	c.mu.Unlock()
	fn(codex.Notification{Method: method, Params: json.RawMessage(params)})
}

// TestRemoteTurnMirrorsNativeActivity is the happy path of bead
// agent-message-queue-611.40: while a remote-mode turn runs, the native
// session's streamed text and tool call reach the ACP session as
// session/update notifications, and the final answer ends the turn without
// repeating the streamed text.
func TestRemoteTurnMirrorsNativeActivity(t *testing.T) {
	root, err := os.MkdirTemp("", "acpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "extensions", "remote")
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	rt := &codexFake{Runtime: fake.New("fake", "e_1"), observed: make(chan struct{}, 1)}
	ep.Register(rt)
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetActivityHandler(activity.MirrorHandler(ep))
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served; _ = ep.Close(); _ = store.Close() })

	server := acp.NewServer(acp.Config{Root: root, RemoteTarget: "fake", RemoteNative: "fake", TurnTimeout: 5 * time.Second, HeartbeatInterval: time.Minute}, "test")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { _ = server.Serve(inR, outW); _ = outW.Close() }()
	t.Cleanup(func() { _ = inW.Close() })
	lines := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				lines <- msg
			}
		}
		close(lines)
	}()
	send := func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		select {
		case msg, ok := <-lines:
			if !ok {
				t.Fatal("ACP output closed")
			}
			return msg
		case <-time.After(5 * time.Second):
			t.Fatal("no ACP output within 5s")
		}
		return nil
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)
	read()
	send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`)
	sessionID := read()["result"].(map[string]any)["sessionId"].(string)
	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"list files"}]}}`, sessionID))

	update := func(msg map[string]any) map[string]any {
		if msg["method"] != "session/update" {
			return nil
		}
		params := msg["params"].(map[string]any)
		if params["sessionId"] != sessionID {
			t.Fatalf("session/update for %v, want %s", params["sessionId"], sessionID)
		}
		return params["update"].(map[string]any)
	}
	ref := ""
	for ref == "" {
		if u := update(read()); u != nil && u["sessionUpdate"] == "agent_thought_chunk" {
			text := u["content"].(map[string]any)["text"].(string)
			ref = strings.TrimSuffix(text[strings.LastIndex(text, " ")+1:], ".")
		}
	}
	_, _, requestID, err := protocol.DecodeRef(ref)
	if err != nil {
		t.Fatalf("submitted thought names no request: %v", err)
	}
	select {
	case <-rt.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("the activity stream never observed the target")
	}

	rt.send("item/agentMessage/delta", `{"threadId":"fake","turnId":"t1","itemId":"m1","delta":"hello "}`)
	rt.send("item/agentMessage/delta", `{"threadId":"fake","turnId":"t1","itemId":"m1","delta":"world"}`)
	rt.send("item/started", `{"threadId":"fake","turnId":"t1","item":{"type":"commandExecution","id":"c1","command":"ls","status":"inProgress"}}`)
	rt.send("item/completed", `{"threadId":"fake","turnId":"t1","item":{"type":"commandExecution","id":"c1","command":"ls","status":"completed","aggregatedOutput":"a.txt\n"}}`)
	rt.send("item/completed", `{"threadId":"fake","turnId":"t1","item":{"type":"agentMessage","id":"m1","text":"hello world"}}`)
	if !rt.Complete(requestID, "hello world") {
		t.Fatal("no running request to complete")
	}

	var kinds []string
	text := ""
	var result map[string]any
	for result == nil {
		msg := read()
		if msg["id"] == float64(3) {
			result = msg["result"].(map[string]any)
			break
		}
		u := update(msg)
		if u == nil || u["sessionUpdate"] == "agent_thought_chunk" {
			continue
		}
		kind := u["sessionUpdate"].(string)
		kinds = append(kinds, kind)
		switch kind {
		case "agent_message_chunk":
			text += u["content"].(map[string]any)["text"].(string)
		case "tool_call", "tool_call_update":
			if u["toolCallId"] != "c1" {
				t.Fatalf("%s for %v, want c1", kind, u["toolCallId"])
			}
		}
	}
	if got := strings.Join(kinds, ","); got != "agent_message_chunk,tool_call,tool_call_update" {
		t.Fatalf("mirrored updates = %s, want the streamed text, then the tool call, and no repeated answer", got)
	}
	if text != "hello world" {
		t.Fatalf("streamed text = %q, want %q", text, "hello world")
	}
	if result["stopReason"] != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %v, want end_turn: %v", result["stopReason"], result)
	}
	if state := result["_meta"].(map[string]any)["remote"].(map[string]any)["state"]; state != "completed" {
		t.Fatalf("remote state = %v, want completed", state)
	}
}
