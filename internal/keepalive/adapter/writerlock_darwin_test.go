//go:build darwin

package adapter

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestFcntlLockHeldSeesFlockWithoutAcquiring(t *testing.T) {
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
	held, err := fcntlLockHeld(path)
	if err != nil {
		t.Fatalf("fcntlLockHeld: %v", err)
	}
	if !held {
		t.Fatal("F_GETLK did not see a flock holder")
	}
	idle := t.TempDir() + "/idle.lock"
	if err := os.WriteFile(idle, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	idleHeld, err := fcntlLockHeld(idle)
	if err != nil {
		t.Fatalf("idle fcntlLockHeld: %v", err)
	}
	if idleHeld {
		t.Fatal("idle lock file reported held")
	}
}
