package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func harnessRoot(t *testing.T) (string, *fsq.DeliveryRoot) {
	t.Helper()
	dir := t.TempDir()
	if err := fsq.EnsureRootDirs(dir); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"version":1,"agents":["claude","codex"]}`)
	if err := os.WriteFile(filepath.Join(dir, "meta", "config.json"), config, 0o600); err != nil {
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
	if repaired := fsq.RepairMailboxLayoutForAgents(root, []string{"claude", "codex"}); repaired.Status != "repaired" {
		t.Fatalf("repair conformance root: %#v", repaired)
	}
	return dir, root
}
