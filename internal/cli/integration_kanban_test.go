package cli

import (
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// A bad --url scheme is a usage error before the bridge prints that it started (audit 2026-11-10).
func TestKanbanBridgeRejectsBadURLBeforeStart(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runKanbanBridge([]string{"--root", root, "--me", "alice", "--url", "http://127.0.0.1:1/ws", "--json"})
	})
	if GetExitCode(err) != ExitUsage {
		t.Fatalf("exit=%d err=%v, want usage", GetExitCode(err), err)
	}
	if strings.Contains(stdout, "started") {
		t.Fatalf("bridge printed a start line before validation: %q", stdout)
	}
}
