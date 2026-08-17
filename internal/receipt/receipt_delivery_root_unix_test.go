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

func TestReadDeliveryRootRejectsSymlink(t *testing.T) {
	rootPath := t.TempDir()
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

	r := New("msg_symlink_root", "p2p/claude__codex", "claude", "codex", StageDrained, "")
	if err := EmitDeliveryRoot(root, r); err != nil {
		t.Fatalf("EmitDeliveryRoot: %v", err)
	}
	rel := filepath.Join("agents", "codex", "receipts")
	if err := os.Symlink(r.filename(), filepath.Join(rootPath, rel, "linked.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err = ReadDeliveryRoot(root, filepath.Join(rel, "linked.json"))
	if err == nil {
		t.Fatal("expected symlink receipt to be rejected")
	}
}

func TestWaitForDeliveryRootTimeoutZeroWaitsUntilReceipt(t *testing.T) {
	rootPath := t.TempDir()
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

	done := make(chan error, 1)
	go func() {
		_, err := WaitForDeliveryRoot(root, "msg_zero_root", "codex", StageDrained, 0, 20*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("timeout 0 returned immediately: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	r := New("msg_zero_root", "p2p/claude__codex", "claude", "codex", StageDrained, "")
	if err := EmitDeliveryRoot(root, r); err != nil {
		t.Fatalf("EmitDeliveryRoot: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForDeliveryRoot(timeout 0): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForDeliveryRoot(timeout 0) did not observe the receipt")
	}
}
