package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// writeClaudeSessionIn writes the session registry entry of this process for
// a Claude Code session in cwd, by a rename, as the harness replaces it.
func writeClaudeSessionIn(t *testing.T, home, cwd string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "sess-abc", "kind": "interactive", "cwd": cwd, "status": "idle"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// Session grant, Claude Code: grant_info offers a grant once the session's
// prompt hook reported a mode that asks, with the text limit of a hook's
// context, and names state_in_workspace as its own reason when the session's
// working directory holds the endpoint's state.
func TestGrantInfoForClaudeSession(t *testing.T) {
	home, stateDir := t.TempDir(), t.TempDir()
	writeClaudeSessionIn(t, home, t.TempDir())
	cfg, err := json.Marshal(map[string]any{"pid": os.Getpid(), "home": home})
	if err != nil {
		t.Fatal(err)
	}
	att, err := claude.Factory(context.Background(), registry.FactoryConfig{StateDir: stateDir, Target: "cc-1", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close() })
	ep.Register(att)
	ls := &linkSet{stateDir: stateDir, ep: ep, running: map[string]*linkRun{}}
	b := linkio.Binding{TargetID: "cc-1"}

	if ok, reason, _ := ls.grantInfoFor(b); ok || reason != linkio.GrantReasonModeUnreadable {
		t.Fatalf("grant_info with no prompt report = %v %q, want %q", ok, reason, linkio.GrantReasonModeUnreadable)
	}
	ep.GrantedPrompt(core.PromptReport{TargetID: "cc-1", Instance: att.(core.PromptHandover).PromptInstance(),
		Prompt: core.Prompt{NativeSession: "sess-abc", Mode: "default"}})
	if ok, reason, limit := ls.grantInfoFor(b); !ok || reason != "" || limit != claude.GrantedTextLimit {
		t.Fatalf("grant_info after an asking report = %v %q %d, want a grant with limit %d", ok, reason, limit, claude.GrantedTextLimit)
	}
	writeClaudeSessionIn(t, home, filepath.Dir(stateDir))
	if ok, reason, _ := ls.grantInfoFor(b); ok || reason != linkio.GrantReasonStateInWorkspace {
		t.Fatalf("grant_info with the state in the working directory = %v %q, want %q", ok, reason, linkio.GrantReasonStateInWorkspace)
	}
}
