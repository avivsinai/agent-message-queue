//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Paths without a bound terminal authority still fail closed at each write:
// neither an unreadable lock nor a proven replacement can authorize input.
func TestPlatformWriteGateRejectsUnreadableAndReplacedWakeLock(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	lockPath := filepath.Join(root, "agents", "codex", ".wake.lock")
	if err := os.WriteFile(lockPath, []byte(`{"generation":"mine","tty":"unknown"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &wakeConfig{
		root:               root,
		me:                 "codex",
		terminalGeneration: "mine",
		terminalTTY:        "unknown",
	}
	if !authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate refused the incumbent's own readable lock")
	}

	if err := os.Chmod(lockPath, 0); err != nil {
		t.Fatal(err)
	}
	if authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate authorized a write with an unreadable lock")
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(lockPath, []byte(`{"generation":"replacement","tty":"unknown"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate authorized a write against a replacement's lock")
	}
}
