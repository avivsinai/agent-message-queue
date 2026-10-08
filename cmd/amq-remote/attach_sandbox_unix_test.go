//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bead agent-message-queue-611.65 (seen live in Codex 0.161.0): a sandboxed
// tool call cannot stat the daemon socket, and attach said the thread was
// not loaded. It must name the blocked socket instead.
func TestSelfCandidateNamesABlockedCodexSocket(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".codex", "app-server-control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(control, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(control, 0o700) })
	t.Setenv("HOME", home)
	t.Setenv("CODEX_THREAD_ID", "01a11a2d-7afd-7452-a12c-614e298df6de")

	_, err := selfCandidate(t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside the sandbox") {
		t.Fatalf("selfCandidate error = %v, want the blocked daemon socket named", err)
	}
}
