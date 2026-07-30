//go:build darwin

package cli

import (
	"os"
	"testing"
)

func TestRetireWakeUsesDarwinCooperativeControlAndPreservesTarget(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	lock := inspectWakeLock(root, "codex").Lock
	lock.ControlSocket = wakeControlSocketPath(root, "codex", lock.Generation)
	writeWakeLockForTest(t, root, "codex", lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})

	cleanup, stopRequested, markStopped, err := startWakeControlListener(root, "codex", lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() {
		<-stopRequested
		markStopped()
	}()

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("wake lock still exists: %v", err)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("saved target was not preserved: %v", err)
	}
}
