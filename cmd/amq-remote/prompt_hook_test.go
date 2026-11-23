package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Session grant, Claude Code: the prompt hook finds the endpoint that serves
// its session from the attachment's locator, reports the prompt's mode there,
// and prints the text the endpoint gives for a granted stub as the prompt's
// added context.
func TestPromptHookReportsToTheEndpointThatServesTheSession(t *testing.T) {
	home := t.TempDir()
	writeClaudeSessionIn(t, home, t.TempDir())
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
	ep := core.New(core.Config{Store: store})
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

	hook := func(prompt string) string {
		t.Helper()
		in, err := json.Marshal(map[string]string{"session_id": "sess-abc", "permission_mode": "default", "prompt": prompt, "hook_event_name": "UserPromptSubmit"})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := promptHook(home, bytes.NewReader(in), &out); code != 0 {
			t.Fatalf("promptHook exit = %d, want 0", code)
		}
		return out.String()
	}

	if out := hook("hello"); out != "" {
		t.Fatalf("a plain prompt printed %q, want nothing", out)
	}
	if asks, why := att.(core.PermissionReader).SessionAsks(); !asks {
		t.Fatalf("SessionAsks after the hook's report = false (%s), want the reported mode", why)
	}

	const id = "44444444-4444-4444-8444-444444444401"
	server.SetPromptHandler(func(r ipc.PromptRequest) ipc.PromptReply {
		if r.StubID != id || r.TargetID != "cc-1" || r.Instance != att.(core.PromptHandover).PromptInstance() || r.Mode != "default" {
			return ipc.PromptReply{}
		}
		return ipc.PromptReply{Text: "the task"}
	})
	out := hook("<cross-session-message from=\"amq-cc-1\">\nAMQ granted task " + id + "\n</cross-session-message>")
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" || got.HookSpecificOutput.AdditionalContext != claude.GrantedContext(id, "the task") {
		t.Fatalf("hook output = %+v, want the granted context of the task", got.HookSpecificOutput)
	}
}
