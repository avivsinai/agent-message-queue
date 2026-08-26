//go:build linux

package adapter

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestProcLocksSeesFlockWithoutAcquiring(t *testing.T) {
	path := t.TempDir() + "/held.lock"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) })
	time.Sleep(10 * time.Millisecond)
	held, err := procLocksHeld(path)
	if err != nil {
		t.Fatalf("procLocksHeld: %v", err)
	}
	if !held {
		dump, _ := os.ReadFile("/proc/locks")
		t.Fatalf("/proc/locks did not see a FLOCK ADVISORY WRITE on the temp file; /proc/locks=\n%s", dump)
	}
	idle := t.TempDir() + "/idle.lock"
	if err := os.WriteFile(idle, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	idleHeld, err := procLocksHeld(idle)
	if err != nil {
		t.Fatalf("idle procLocksHeld: %v", err)
	}
	if idleHeld {
		t.Fatal("idle lock file reported held")
	}
}
