//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMalformedPreviouslyBoundWakeLockForTest(t *testing.T, root, me string) (string, []byte, os.FileInfo) {
	t.Helper()
	target := mustNewWakeTargetForTest(t, root, me, writeExecutableForTest(t, "malformed-lock-injector"), []string{"exec"})
	lock := bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Generation: "0123456789abcdef0123456789abcdef",
	}, target)
	lock.StateGeneration = lock.Generation
	lock.StateDigest = lock.TargetDigest
	path := writeWakeLockForTest(t, root, me, lock)
	if err := os.WriteFile(path, []byte(`{"pid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(-3*time.Second), time.Now().Add(-3*time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw, info
}

func TestMalformedPreviouslyBoundWakeLockQuarantinesOnGenericAcquire(t *testing.T) {
	root := secureTempDirForTest(t)
	path, raw, info := writeMalformedPreviouslyBoundWakeLockForTest(t, root, "codex")

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if err != nil {
		t.Fatalf("generic acquisition after malformed lock quarantine: %v", err)
	}
	t.Cleanup(cleanup)
	assertExactWakeQuarantineForTest(t, filepath.Dir(path), ".wake.lock.quarantined.", raw, info)
}

func TestValidUnboundP2aStaleWakeLockIsReplaced(t *testing.T) {
	root := secureTempDirForTest(t)
	path := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 4242})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if err != nil {
		t.Fatalf("acquire after valid unbound P2a stale lock: %v", err)
	}
	t.Cleanup(cleanup)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("valid unbound P2a stale lock was not replaced")
	}
	var replacement wakeLock
	if err := json.Unmarshal(after, &replacement); err != nil {
		t.Fatalf("decode replacement lock: %v", err)
	}
	if replacement.PID != os.Getpid() || replacement.StateGeneration != "" || replacement.StateDigest != "" {
		t.Fatalf("replacement = %#v, want a new unbound P2a lock", replacement)
	}
}
