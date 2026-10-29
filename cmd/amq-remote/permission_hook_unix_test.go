//go:build !windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestMain lets a test run this binary's own main as a child process. The
// tests pass a temp home; CLAUDE_CONFIG_DIR or CODEX_HOME would move the
// settings files out of it.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	_ = os.Unsetenv("CODEX_HOME")
	if os.Getenv("AMQ_REMOTE_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// Live probe 2026-09-30: Claude sends SIGTERM when the terminal rejects and
// at the hook timeout, then SIGKILL about 1 s later. The real
// `claude permission-hook` traps TERM, records answered_elsewhere, and
// exits 0 with no decision.
func TestPermissionHookSIGTERMRecordsAnsweredElsewhere(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-1"
	if _, err := claude.PinApprovals(home, sid, "share-1", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude", "sessions", "amq-approve", sid)
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", "p-1"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]any{"session_id": sid, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}})
	cmd := exec.Command(os.Args[0], "claude", "permission-hook", "--wait", "60")
	cmd.Env = append(os.Environ(), "AMQ_REMOTE_TEST_MAIN=1", "HOME="+home)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	id := ""
	for deadline := time.Now().Add(5 * time.Second); id == ""; time.Sleep(5 * time.Millisecond) {
		entries, _ := os.ReadDir(filepath.Join(dir, "requests"))
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && strings.HasPrefix(name, "cc-") {
				id = name
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the hook raised no request")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("hook exit = %v, want 0", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the hook did not exit on SIGTERM")
	}
	if stdout.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", stdout.String())
	}
	var res map[string]string
	if raw, err := os.ReadFile(filepath.Join(dir, "resolved", id+".json")); err != nil || json.Unmarshal(raw, &res) != nil || res["outcome"] != "answered_elsewhere" {
		t.Fatalf("resolved = %v (%v), want answered_elsewhere", res, err)
	}
}

// On Buzz Desktop the agent, not the hook, holds the key: the owner pins the
// owner, the agent's pubkey and its DM channel with install-approval-hook
// --desktop, and the hook allows a Bash call on the owner's signed ✅ on the
// agent's approval message plus the agent's signed grant, with no relay read.
func TestDesktopPinAllowsOnTheOwnersReactionAndTheAgentsGrant(t *testing.T) {
	home := t.TempDir()
	const sid, channel = "sess-1", "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	var owner, agent [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(agent[:])
	ownerHex, agentHex := nostr.GetPublicKey(owner).Hex(), nostr.GetPublicKey(agent).Hex()
	if code := installApprovalHook(home, []string{"--desktop", "--owner", ownerHex, "--body", agentHex, "--channel", channel}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("install-approval-hook --desktop = %d", code)
	}
	pin := claude.PermissionHookPin(home)
	if !pin.DesktopComplete() || pin.Owner != ownerHex || pin.Body != agentHex || pin.Channel != channel {
		t.Fatalf("installed pin = %+v, want the Desktop owner, agent and channel", pin)
	}
	// serve's pin for an adapter with approve that no share serves.
	if _, err := claude.PinApprovals(home, sid, "", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude", "sessions", "amq-approve", sid)
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", "p-1"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]any{"session_id": sid, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}})
	var out bytes.Buffer
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		claude.RunPermissionHook(home, bytes.NewReader(stdin), &out, io.Discard, make(chan struct{}), time.Minute, allowConfig(pin))
	}()
	var req struct {
		InteractionID string `json:"interaction_id"`
		ActionHash    string `json:"action_hash"`
		Preview       string `json:"preview"`
		Approvable    bool   `json:"approvable"`
	}
	for deadline := time.Now().Add(5 * time.Second); req.InteractionID == ""; time.Sleep(5 * time.Millisecond) {
		entries, _ := os.ReadDir(filepath.Join(dir, "requests"))
		for _, e := range entries {
			if raw, err := os.ReadFile(filepath.Join(dir, "requests", e.Name())); err == nil {
				_ = json.Unmarshal(raw, &req)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook raised no request")
		}
	}
	if !req.Approvable {
		t.Fatalf("request = %+v, want an approvable call", req)
	}
	msg := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Tags: nostr.Tags{{"h", channel}, {"p", ownerHex}}, Content: buzzio.DesktopApprovalText(req.Preview)}
	if err := msg.Sign(agent); err != nil {
		t.Fatal(err)
	}
	react := nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	grant, err := buzzio.SignDesktopGrant(agent, msg.ID.Hex(), react.ID.Hex(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal(buzzio.DesktopEvidence{Reaction: react, Message: msg, Grant: grant})
	answer, _ := json.Marshal(map[string]any{"interaction_id": req.InteractionID, "action_hash": req.ActionHash, "option": "allow",
		"at": protocol.FormatTime(time.Now()), "evidence": json.RawMessage(evidence)})
	if err := os.MkdirAll(filepath.Join(dir, "answers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "answers", req.InteractionID+".json"), answer, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not decide")
	}
	if got := strings.TrimSpace(out.String()); got != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` {
		t.Fatalf("hook printed %q, want allow", got)
	}
}
