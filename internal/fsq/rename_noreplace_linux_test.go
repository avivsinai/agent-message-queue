//go:build linux

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Issue #1034: on a FUSE mount renameat2 refuses RENAME_NOREPLACE with EINVAL
// while a plain rename works, so send and drain failed with "invalid argument".
func TestRenameNoReplaceFallsBackWhenFlagUnsupported(t *testing.T) {
	saved := renameat2
	t.Cleanup(func() { renameat2 = saved })
	renameat2 = func(oldDirFD int, oldName string, newDirFD int, newName string, flags uint) error {
		if flags != 0 {
			return unix.EINVAL
		}
		return saved(oldDirFD, oldName, newDirFD, newName, 0)
	}

	base := setupClaimAgent(t, "alice")
	root := openDeliveryRootForTest(t, base)
	if _, err := DeliverToInbox(root, "alice", "fuse.md", []byte("hello")); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if err := MoveNewToCur(root, "alice", "fuse.md"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The guard still refuses to replace an existing destination.
	writeClaimMessage(t, base, "alice", "fuse.md")
	var collision *ClaimCollisionError
	if err := MoveNewToCur(root, "alice", "fuse.md"); !errors.As(err, &collision) {
		t.Fatalf("claim onto an existing cur = %T %v, want *ClaimCollisionError", err, err)
	}
	got, err := os.ReadFile(filepath.Join(AgentInboxCur(base, "alice"), "fuse.md"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("retained cur = %q, %v; the fallback must not replace it", got, err)
	}

	// A session directory publishes through the exclusive-placeholder path.
	child, err := root.PublishInitializedDirectChildExclusive("session", 0o700, func(child *DeliveryRoot) error {
		return child.EnsureAgentDirs("bob")
	})
	if err != nil {
		t.Fatalf("publish session directory: %v", err)
	}
	_ = child.Close()
	if info, err := os.Stat(AgentInboxNew(filepath.Join(base, "session"), "bob")); err != nil || !info.IsDir() {
		t.Fatalf("published session inbox: info=%v err=%v", info, err)
	}
}
