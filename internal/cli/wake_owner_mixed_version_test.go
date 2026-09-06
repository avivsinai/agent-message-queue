//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestMixedVersionOldLiveUnboundLockUsesNewReaderFallback(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-unbound-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"legacy"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	writeWakeLockExactForTest(t, root, "codex", lock)
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	selection, err := readWakeStateSelectionForInspection(root, "codex", inspectWakeLock(root, "codex"))
	if err != nil || !selection.TargetPresent || !sameWakeTarget(selection.Target, target) {
		t.Fatalf("unbound old-lock selection=%#v err=%v", selection, err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("new reader rewrote the old unbound lock")
	}
}
