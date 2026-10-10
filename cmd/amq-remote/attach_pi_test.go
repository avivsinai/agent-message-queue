package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// Regression (local e2e F1, bead y2v): attach --self inside a pi (Amit) chat
// could not find the chat. The chat runs its tools as children of the pi
// process its bridge names in bridge.liveness, so the process ancestry
// identifies it, with the chat's own session id. Without a live chat no pi
// candidate is chosen (the test itself may run inside another harness, so
// only the pi outcome is asserted). Process ancestry is read with ps: the
// boundary of the defect.
func TestSelfCandidateFindsThePiChatItRunsIn(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	root := t.TempDir()
	stateDir := filepath.Join(root, stateDirName)
	if cand, err := selfCandidate(root, stateDir); err == nil && cand.Kind == "pi" {
		t.Fatalf("no pi chat runs, yet selfCandidate chose %+v", cand)
	}
	dir := filepath.Join(root, "agents", "chat-1", "extensions", "pi-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(map[string]any{
		"protocol": pi.ProtocolV1, "live": true, "at": time.Now().UTC().Format(time.RFC3339),
		"pid": os.Getppid(), "surface": "tui", "bridge_revision": 4, "session_id": "sess-1",
	})
	if err := os.WriteFile(filepath.Join(dir, "bridge.liveness"), live, 0o600); err != nil {
		t.Fatal(err)
	}
	cand, err := selfCandidate(root, stateDir)
	if err != nil || cand.Kind != "pi" || cand.Target != "pi:chat-1" {
		t.Fatalf("selfCandidate = %+v, %v; want the pi chat", cand, err)
	}
	if native, err := selfNativeSession(cand); err != nil || native != "sess-1" {
		t.Fatalf("native session = %q, %v; want sess-1", native, err)
	}
}
