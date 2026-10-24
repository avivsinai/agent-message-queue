package agentidentity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-t97.4 (Pro findings 6 and 7): two starts must
// leave one coherent record, and a publication must never recreate a
// mailbox that cleanup removed.
func TestPublishReplacesAndNeverCreatesAMailbox(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "reviewer"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deliveryRoot.Close() }()

	start := time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)
	for _, rec := range []Record{New("reviewer", "claude", "", start), New("reviewer", "agy", "amq-acp", start.Add(time.Minute))} {
		if err := Publish(deliveryRoot, rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(root, "reviewer")
	if err != nil || got != New("reviewer", "agy", "amq-acp", start.Add(time.Minute)) {
		t.Fatalf("record = %+v err=%v, want the second start", got, err)
	}

	if err := Publish(deliveryRoot, New("cleaned", "claude", "", start)); err == nil {
		t.Fatal("publish into a missing mailbox succeeded")
	}
	if _, err := os.Lstat(filepath.Join(root, "agents", "cleaned")); !os.IsNotExist(err) {
		t.Fatalf("publish created the missing mailbox: %v", err)
	}
}
