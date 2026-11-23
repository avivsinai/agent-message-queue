//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// fakeClaudeSocket writes, under home, what Claude Code writes for the idle
// session sess-abc of this process in cwd: its registry entry with its
// cross-session socket, and the peer key file the harness mints for that
// socket. It returns the frames the socket receives.
func fakeClaudeSocket(t *testing.T, home, cwd string) <-chan claude.Frame {
	t.Helper()
	pid := os.Getpid()
	// A short directory: a unix socket path is bounded.
	sockDir, err := os.MkdirTemp("", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, fmt.Sprintf("%d.sock", pid))
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	frames := make(chan claude.Frame, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			raw, _ := io.ReadAll(conn)
			_ = conn.Close()
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var f claude.Frame
			if json.Unmarshal([]byte(lines[len(lines)-1]), &f) == nil {
				frames <- f
			}
		}
	}()

	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := json.Marshal(map[string]any{"pid": pid, "sessionId": "sess-abc", "kind": "interactive",
		"cwd": cwd, "status": "idle", "messagingSocketPath": sockPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sockPath))
	key := filepath.Join(dir, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:])))
	if err := os.WriteFile(key, []byte(`{"peerToken":"00000000000000000000000000000000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return frames
}

// appendSessionTranscript appends entries to the transcript of session
// sess-abc in cwd, where Claude Code writes it: the project directory is cwd
// with every character that is not a letter or a digit replaced by '-'.
func appendSessionTranscript(t *testing.T, home, cwd string, entries ...map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, e := range entries {
		e["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	f, err := os.OpenFile(filepath.Join(dir, "sess-abc.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(b.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func messageEntry(role, content string) map[string]any {
	return map[string]any{"type": role, "message": map[string]any{"role": role, "content": content}}
}

// Session grant, Claude Code, end to end through the installed command: a
// granted link task goes to the session as a stub, the session's prompt hook
// takes the task's text from the endpoint over the local socket and prints
// it as the prompt's context, and the task completes with the session's
// answer once the transcript shows that context and the Stop hook ran.
func TestPromptHookCarriesAGrantedLinkTaskEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(binding.EnvPath, "")
	cwd := t.TempDir()
	frames := fakeClaudeSocket(t, home, cwd)
	// The session answered a prompt before the task arrives.
	appendSessionTranscript(t, home, cwd, messageEntry("user", "an earlier prompt"), messageEntry("assistant", "an earlier answer"))

	// A short state directory: it holds the endpoint's socket.
	stateDir, err := os.MkdirTemp("", "amqh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	cfg, err := json.Marshal(map[string]any{"pid": os.Getpid(), "home": home})
	if err != nil {
		t.Fatal(err)
	}
	att, err := claude.Factory(context.Background(), registry.FactoryConfig{StateDir: stateDir, Target: "cc-1", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Consent: func(string, string) bool { return true }})
	ep.SetGrant(func(string, string, string) string { return "" })
	t.Cleanup(func() { _ = ep.Close() })
	ep.Register(att)
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	server.SetPromptHandler(promptHandler(ep))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	hook := func(sub string, input map[string]string) string {
		t.Helper()
		in, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := claudeSubcommand([]string{sub}, bytes.NewReader(in), &out); code != 0 {
			t.Fatalf("claude %s exit = %d, want 0", sub, code)
		}
		return out.String()
	}
	prompt := func(text string) string {
		return hook("prompt-hook", map[string]string{"session_id": "sess-abc", "permission_mode": "default", "prompt": text, "hook_event_name": "UserPromptSubmit"})
	}

	// The session's own prompt reports its mode; it prints nothing.
	if out := prompt("hello"); out != "" {
		t.Fatalf("a plain prompt printed %q, want nothing", out)
	}

	const taskText = "list the files of this directory"
	const requestID = "55555555-5555-4555-8555-555555555501"
	key := requests.Key{CreatorHost: "link-aaaaaaaaaaaaaaaa", TargetID: "cc-1", RequestID: requestID}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: requestID, TargetID: "cc-1",
		Epoch: claude.SentinelUnpinned, NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
		Input: &protocol.SubmitInput{Text: taskText, Deliver: protocol.DeliverTurn, Busy: protocol.BusyReject}}
	src := core.Source{Host: key.CreatorHost, Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: key.CreatorHost},
		Shared: []string{"cc-1"}, NativeSession: "sess-abc", Credential: "cred-1",
		GrantDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000005"}
	out, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(protocol.Reply); r.Outcome.Code != "" || r.Snapshot.State != protocol.StateRunning {
		t.Fatalf("granted submit = %s %q %q, want running", r.Snapshot.State, r.Outcome.Code, r.Outcome.Message)
	}

	var frame claude.Frame
	select {
	case frame = <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("the session's socket received no frame")
	}
	envelope := frame.Message.Content
	if strings.Contains(envelope, taskText) {
		t.Fatalf("the frame carries the task text: %q", envelope)
	}
	id, ok := claude.StubMsgID(envelope)
	if !ok || id != frame.MsgID {
		t.Fatalf("StubMsgID(frame) = %q %v, want the stub of the frame's msg_id %s", id, ok, frame.MsgID)
	}

	// The stub raises a prompt in the session: the hook takes the text.
	printed := prompt(envelope)
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(printed)), &got); err != nil {
		t.Fatalf("prompt hook output %q: %v", printed, err)
	}
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" || got.HookSpecificOutput.AdditionalContext != claude.GrantedContext(id, taskText) {
		t.Fatalf("prompt hook output = %+v, want the granted context of the task", got.HookSpecificOutput)
	}
	if rec, _, err := store.Get(key); err != nil || !rec.GrantTextGiven || rec.State != protocol.StateRunning {
		t.Fatalf("record after the hand-over = %s given=%v (%v), want running with grant_text_given", rec.State, rec.GrantTextGiven, err)
	}

	// The transcript shows the stub's prompt, the context the hook printed,
	// and the answer; the Stop hook ends the turn.
	appendSessionTranscript(t, home, cwd,
		map[string]any{"type": "user",
			"message": map[string]any{"role": "user", "content": "Another Claude session sent a message: " + envelope + "  This came from another Claude session — not typed by your user."},
			"origin":  map[string]any{"kind": "peer", "from": "unknown", "msg_id": id, "name": "amq-remote"}},
		map[string]any{"type": "attachment", "attachment": map[string]any{"type": "hook_additional_context", "hookName": "UserPromptSubmit",
			"hookEvent": "UserPromptSubmit", "toolUseID": "toolu_1", "content": []string{got.HookSpecificOutput.AdditionalContext}}},
		messageEntry("assistant", "a.txt b.txt"))
	hook("stop-hook", map[string]string{"session_id": "sess-abc", "hook_event_name": "Stop", "last_assistant_message": "a.txt b.txt"})

	// The adapter reads the transcript every 500 ms.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec, _, err := store.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State == protocol.StateCompleted && rec.Result != nil && rec.Result.Text == "a.txt b.txt" {
			return
		}
		if rec.State != protocol.StateRunning || time.Now().After(deadline) {
			t.Fatalf("record = %s reason %q result %+v, want completed with the answer", rec.State, rec.RefusalReason, rec.Result)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
