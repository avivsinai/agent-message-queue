//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// TestMain lets a test run this binary's own main as a child process.
func TestMain(m *testing.M) {
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
