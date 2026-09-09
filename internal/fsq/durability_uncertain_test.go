package fsq_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// TestDeliverToInboxCommittedDurabilityUncertain pins bead 611.22.14 (B10):
// a directory-sync failure after the tmp -> new rename must surface as a
// CommittedDurabilityError whose FinalPath names the delivered message —
// committed-but-uncertain, never a plain retryable failure, because a blind
// retry with a fresh identifier would duplicate an artifact already visible
// in the recipient's inbox.
func TestDeliverToInboxCommittedDurabilityUncertain(t *testing.T) {
	dir := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(dir)
	if err != nil {
		t.Fatalf("snapshot delivery root: %v", err)
	}
	root, err := fsq.OpenDeliveryRoot(dir, identity)
	if err != nil {
		t.Fatalf("open delivery root: %v", err)
	}
	defer func() { _ = root.Close() }()

	newDir := filepath.Join(dir, "agents", "bob", "inbox", "new")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatalf("precreate recipient inbox: %v", err)
	}
	syncs := 0
	root.SetSyncDirFaultForTest(func(d string) error {
		if strings.HasSuffix(d, filepath.Join("inbox", "new")) {
			syncs++
			if syncs == 1 {
				return errors.New("injected sync fault")
			}
		}
		return nil
	})

	_, derr := fsq.DeliverToInbox(root, "bob", "m1.md", []byte("hello"))
	var committed *fsq.CommittedDurabilityError
	if !errors.As(derr, &committed) {
		t.Fatalf("deliver err = %v, want CommittedDurabilityError", derr)
	}
	if committed.FinalPath == "" {
		t.Fatal("committed error carries no FinalPath")
	}
	if committed.Recipient != "bob" {
		t.Fatalf("committed recipient = %q, want bob", committed.Recipient)
	}
	if _, statErr := os.Stat(filepath.Join(newDir, "m1.md")); statErr != nil {
		t.Fatalf("committed artifact not visible at FinalPath %s: %v", committed.FinalPath, statErr)
	}
	// The record can be safely republished as the same immutable artifact: the
	// fault clears and a second delivery of the SAME filename lands at the
	// same path, never a duplicate under a new name.
	path2, derr2 := fsq.DeliverToInbox(root, "bob", "m1.md", []byte("hello"))
	if derr2 != nil {
		t.Fatalf("re-deliver with fault cleared: %v", derr2)
	}
	if path2 != committed.FinalPath {
		t.Fatalf("re-delivery path %q differs from committed FinalPath %q", path2, committed.FinalPath)
	}
}
