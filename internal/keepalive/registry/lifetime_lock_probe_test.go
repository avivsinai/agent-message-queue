//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package registry

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestProbeLifetimeLockSharedProbeDoesNotCollide pins review-817-r3 P2-1: the
// probe must take LOCK_SH|LOCK_NB, not LOCK_EX|LOCK_NB. With LOCK_EX, two
// concurrent probers collided — a second prober made a genuinely dead row
// read as live (up's reclaim then skipped a dead row and refused to start).
func TestProbeLifetimeLockSharedProbeDoesNotCollide(t *testing.T) {
	base := t.TempDir()
	regPath := filepath.Join(base, "registry.json")

	// Dead row: lock file exists, nobody holds it.
	lockPath := LifetimeLockPath(regPath, "dead-entry")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// A second prober holds SH (as any concurrent doctor --ops / up reclaim
	// does while probing). The first probe must still report not-held — the
	// dead row must not be falsified by a peer prober.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("second prober could not take SH: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) })

	held, err := ProbeLifetimeLock(regPath, "dead-entry")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if held {
		t.Fatalf("dead row read as live while a peer prober holds LOCK_SH — probe is not shared")
	}
}

// TestProbeLifetimeLockDetectsExclusiveOwner pins the other half: a live
// owner holding LOCK_EX (what amq-remote up takes) is still reported held.
func TestProbeLifetimeLockDetectsExclusiveOwner(t *testing.T) {
	base := t.TempDir()
	regPath := filepath.Join(base, "registry.json")

	lockPath := LifetimeLockPath(regPath, "live-entry")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("owner could not take EX: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) })

	held, err := ProbeLifetimeLock(regPath, "live-entry")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !held {
		t.Fatalf("live owner (LOCK_EX) not detected as held")
	}
}
