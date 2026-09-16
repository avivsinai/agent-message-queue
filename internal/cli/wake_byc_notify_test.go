package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// TestBycNotifyNewMessagesRefusesDetachedInbox (byc-b) proves the retained
// inbox is revalidated for canonical identity before each authoritative read.
//
// Before the fix, notifyNewMessages called cfg.retainedInbox.ReadDir() WITHOUT
// calling ValidateCanonical first. A directory swap after admission left the
// retained FD pointing at the detached OLD inbox; messages landing in the
// canonical inbox were never seen. SILENT NON-DELIVERY.
//
// The fix calls ValidateCanonical (via interface assertion) before ReadDir in
// notifyNewMessages. A swap renames the canonical inbox/new away and replaces
// it; the retained FD still points at the OLD inode, so SameFile fails and we
// refuse rather than reading a detached namespace.
//
// Mutation RED: remove the ValidateCanonical call in notifyNewMessages -> the
// swap goes undetected, ReadDir reads the detached OLD inbox (empty), returns
// nil, and the test's refusal assertion fails.
func TestBycNotifyNewMessagesRefusesDetachedInbox(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inboxDir.Close() }()

	cfg := &wakeConfig{
		root:          root,
		me:            "codex",
		retainedInbox: inboxDir,
	}

	// Sanity: before the swap, notifyNewMessages reads the canonical inbox
	// (empty) without error.
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("pre-swap notifyNewMessages: %v", err)
	}

	// Swap the canonical inbox/new: rename the OLD directory away and replace
	// it with a fresh directory at the same path. The retained FD still points
	// at the OLD (now-detached) inode.
	inboxNew := filepath.Join(root, "agents", "codex", "inbox", "new")
	detached := inboxNew + ".detached-byc"
	if err := os.Rename(inboxNew, detached); err != nil {
		t.Fatalf("swap: rename canonical inbox/new away: %v", err)
	}
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		t.Fatalf("swap: mkdir replacement inbox/new: %v", err)
	}

	// notifyNewMessages must REFUSE: the retained inbox no longer matches the
	// canonical namespace. It must NOT silently read the detached OLD inbox.
	err = notifyNewMessages(cfg)
	if err == nil {
		t.Fatal("notifyNewMessages read the detached inbox without error (byc: must revalidate canonical identity before ReadDir and refuse on a swap)")
	}
}
