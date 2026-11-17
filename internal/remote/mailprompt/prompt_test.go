package mailprompt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Regression: #964, the layout repair must never recreate a
// mailbox or inbox that vanished after the anchor check.
func TestMailboxRepairNeverRecreatesItsAnchors(t *testing.T) {
	for name, gone := range map[string]string{"mailbox": ".", "inbox": "inbox"} {
		t.Run(name, func(t *testing.T) {
			root := pinnedTestRoot(t)
			agents, err := root.OpenDirectChild("agents")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agents.Close() }()
			mailbox, err := agents.OpenDirectChild("agent")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mailbox.Close() }()
			inbox, err := mailbox.OpenDirectChild("inbox")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = inbox.Close() }()
			dir := mailbox.Base()
			if err := os.RemoveAll(filepath.Join(dir, gone)); err != nil {
				t.Fatal(err)
			}
			if err := repairMailbox(mailbox, inbox); err == nil {
				t.Fatal("repair succeeded without its anchor")
			}
			if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
				t.Fatalf("the %s was recreated: %v", name, err)
			}
		})
	}
}

// Regression: #964, the repair re-resolved the bound root path, so
// a root replaced, or an agents/ swapped for an outside symlink, after the
// root was pinned had its mailbox completed.
func TestMailboxRepairStaysInThePinnedRoot(t *testing.T) {
	for name, swap := range map[string]func(root, other string) error{
		"root replaced": func(root, other string) error {
			if err := os.Rename(root, filepath.Join(canonicalTempDir(t), "old")); err != nil {
				return err
			}
			return os.Rename(other, root)
		},
		"agents symlinked": func(root, other string) error {
			if err := os.RemoveAll(filepath.Join(root, "agents")); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(other, "agents"), filepath.Join(root, "agents"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := pinnedTestRoot(t)
			other := canonicalTempDir(t)
			if err := fsq.EnsureAgentDirs(other, "agent"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(fsq.AgentReceipts(other, "agent")); err != nil {
				t.Fatal(err)
			}
			if err := swap(root.Base(), other); err != nil {
				t.Fatal(err)
			}
			var refused *notDeliveredError
			if err := completeMailbox(root, "agent"); !errors.As(err, &refused) {
				t.Fatalf("repair after the swap = %v, want a refusal", err)
			}
			if _, err := os.Lstat(fsq.AgentReceipts(root.Base(), "agent")); !os.IsNotExist(err) {
				t.Fatalf("the swapped-in mailbox was completed: %v", err)
			}
		})
	}
}

// pinnedTestRoot pins a fresh root whose agent mailbox lacks receipts/.
func pinnedTestRoot(t *testing.T) *fsq.DeliveryRoot {
	t.Helper()
	dir := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(dir, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsq.AgentReceipts(dir, "agent")); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
