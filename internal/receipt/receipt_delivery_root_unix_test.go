//go:build darwin || linux

package receipt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWaitForDeliveryRootRejectsReplacedRootAlias(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "delivery")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatalf("create delivery root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(rootPath, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	parked := filepath.Join(parent, "delivery-parked")
	if err := os.Rename(rootPath, parked); err != nil {
		t.Fatalf("park delivery root: %v", err)
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatalf("create replacement root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(rootPath, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs replacement: %v", err)
	}
	malicious := New("msg_replaced", "", "attacker", "codex", StageDrained, "")
	if err := Emit(rootPath, malicious); err != nil {
		t.Fatalf("emit replacement receipt: %v", err)
	}

	_, err = WaitForDeliveryRoot(root, malicious.MsgID, malicious.Consumer, malicious.Stage, 100*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("WaitForDeliveryRoot error = %v, want root-change refusal", err)
	}
}
