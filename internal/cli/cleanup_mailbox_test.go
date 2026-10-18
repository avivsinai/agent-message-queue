//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// newMailboxReleaseRootForTest provisions a minimal queue root with two
// registered agents (a chat handle to remove and one that must survive) and
// the reserved human handle's operator inbox.
func newMailboxReleaseRootForTest(t *testing.T) string {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"user", "chat-proj-ab12cd34", "claude"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Version: 1, CreatedUTC: "2026-10-18T00:00:00Z", Agents: []string{"claude", "chat-proj-ab12cd34"}}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), cfg, false); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCleanupMailboxRemovesEmptyMailboxAndDeregisters is the Amit
// releaseChatMailbox happy path: an archived chat's empty mailbox goes away,
// other agents survive, and the handle leaves config.json.
func TestCleanupMailboxRemovesEmptyMailboxAndDeregisters(t *testing.T) {
	root := newMailboxReleaseRootForTest(t)
	agentDir := filepath.Join(root, "agents", "chat-proj-ab12cd34")
	if err := os.MkdirAll(filepath.Join(agentDir, "extensions", "amit-doorbell"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := captureEnvStdout(t, func() error {
		return runCleanup([]string{
			"--root", root, "--mailbox", "chat-proj-ab12cd34", "--yes", "--json",
		})
	})
	if err != nil {
		t.Fatalf("cleanup --mailbox: %v", err)
	}
	if !strings.Contains(out, `"removed": true`) || !strings.Contains(out, `"deregistered": true`) {
		t.Fatalf("json report = %s", out)
	}
	if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("mailbox dir still present: %v", err)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != "claude" {
		t.Fatalf("agents = %v, want [claude] (chat handle deregistered)", cfg.Agents)
	}
	// Other mailboxes survive.
	if _, err := os.Lstat(filepath.Join(root, "agents", "claude")); err != nil {
		t.Fatalf("surviving mailbox removed: %v", err)
	}
}

// TestCleanupMailboxRefusedWhileMailWaits reproduces the field contract: a
// mailbox holding undelivered inbox mail is kept and the command exits 6
// (ExitActionRequired), the code Amit's releaseChatMailbox maps to
// mail-remains. The directory and the config registration survive.
func TestCleanupMailboxRefusedWhileMailWaits(t *testing.T) {
	root := newMailboxReleaseRootForTest(t)
	inboxNew := filepath.Join(root, "agents", "chat-proj-ab12cd34", "inbox", "new")
	if err := os.WriteFile(filepath.Join(inboxNew, "msg_001.md"), []byte("From: claude\n\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runCleanup([]string{"--root", root, "--mailbox", "chat-proj-ab12cd34", "--yes", "--json"})
	if got := GetExitCode(err); got != ExitActionRequired {
		t.Fatalf("exit code = %d, want %d (err = %v)", got, ExitActionRequired, err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "agents", "chat-proj-ab12cd34")); statErr != nil {
		t.Fatalf("mailbox dir removed despite waiting mail: %v", statErr)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents = %v, want both kept (config was mutated)", cfg.Agents)
	}
}

// TestCleanupMailboxRequiresExplicitRootAndProtectsUserHandle covers the
// command's guard rails: no ambient-root operation, and the reserved human
// handle (whose inbox carries the operator gate) is never removable.
func TestCleanupMailboxRequiresExplicitRootAndProtectsUserHandle(t *testing.T) {
	root := newMailboxReleaseRootForTest(t)

	err := runCleanup([]string{"--mailbox", "chat-proj-ab12cd34", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "explicit --root") {
		t.Fatalf("missing explicit --root error = %v", err)
	}

	err = runCleanup([]string{"--root", root, "--mailbox", "user", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "reserved human handle") {
		t.Fatalf("user handle error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "agents", "user")); statErr != nil {
		t.Fatalf("user mailbox touched: %v", statErr)
	}
}
