//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

// TestRunOpsChecks_LocklessCompanionReportsStale pins review-b8 P1-1
// (611.13.2): a companion row whose process is gone (kill -9) holds no
// lifetime lock. checkCompanions probes the lock and projects state
// "stale" with lock "not-held" - the same verdict amq-keepalive doctor
// --ops prints - instead of parroting the recorded state. A held lock
// keeps the recorded state and reports lock "held". Lives in a
// flock-constrained file (review 19:21Z: the runtime OS skip cannot
// prevent compilation of syscall.Flock references on Windows).
func TestRunOpsChecks_LocklessCompanionReportsStale(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	regPath := filepath.Join(home, ".amq-keepalive", "registry.json")
	if err := os.MkdirAll(filepath.Dir(regPath), 0o700); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	// Two rows on separate targets (the registry refuses two agents on one
	// target): a phantom (no lock) and a live row (lock held by this test).
	if _, err := registry.New(regPath).Upsert(registry.Entry{
		Root: root, Agent: "amq-remote", Adapter: "remote", Target: root,
	}); err != nil {
		t.Fatalf("Upsert phantom companion: %v", err)
	}
	liveDir := filepath.Join(root, "live-target")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := registry.New(regPath).Upsert(registry.Entry{
		Root: root, Agent: "amq-remote-2", Adapter: "remote", Target: liveDir,
	})
	if err != nil {
		t.Fatalf("Upsert live companion: %v", err)
	}
	liveLock, err := os.OpenFile(registry.LifetimeLockPath(regPath, live.ID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = liveLock.Close() }()
	if err := syscall.Flock(int(liveLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(liveLock.Fd()), syscall.LOCK_UN) }()

	result := runOpsChecks(root, "test_source", false)
	if len(result.Companions) != 2 {
		t.Fatalf("companions = %#v, want both entries", result.Companions)
	}
	byAgent := map[string]opsCompanion{}
	for _, c := range result.Companions {
		byAgent[c.Agent] = c
	}
	ph := byAgent["amq-remote"]
	if ph.State != "stale" || ph.Lock != "not-held" {
		t.Fatalf("phantom companion = state %q lock %q, want stale/not-held", ph.State, ph.Lock)
	}
	lv := byAgent["amq-remote-2"]
	if lv.State != "active" && lv.State != "attached" {
		t.Fatalf("live companion state = %q, want the recorded live state", lv.State)
	}
	if lv.Lock != "held" {
		t.Fatalf("live companion lock = %q, want held", lv.Lock)
	}
}
