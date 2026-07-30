//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildAuthoritativeClaimForRealObserverTest stubs only the wake helper
// process. Owner liveness remains backed by the real platform observer so
// these tests cover the kqueue/pidfd boundary used by coop preflight.
func buildAuthoritativeClaimForRealObserverTest(
	t *testing.T,
	owner wakeOwner,
	wakeIdentityStart string,
) (root, lockPath, targetPath string) {
	t.Helper()

	const wakePID = 66121
	root = secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "real-observer-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	wakeArgs := []string{
		"/opt/homebrew/bin/amq",
		"wake",
		"--root",
		root,
		"--me",
		"codex",
		"--inject-via",
		injector,
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		TTY:          "/dev/ttys042",
		ProcessStart: wakeIdentityStart,
		BootID:       owner.BootID,
		Executable:   wakeArgs[0],
		Args:         wakeArgs,
		Generation:   "real-observer-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath = writeWakeLockExactForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	targetPath = wakeTargetPath(root, "codex")

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: wakeIdentityStart,
				BootID:     owner.BootID,
				Executable: wakeArgs[0],
				Args:       wakeArgs,
			}
		}
		return inspectWakeProcessPlatform(pid)
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("preflight must not signal any process")
		return nil
	})
	return root, lockPath, targetPath
}

func TestPrepareCoopWakeLockRealObserverDeadOwnerAllowsTakeover(t *testing.T) {
	probe := exec.Command("/bin/sleep", "0")
	if err := probe.Run(); err != nil {
		t.Fatalf("spawn dead-owner probe: %v", err)
	}

	live := currentAuthoritativeOwnerForCoopWakeTest(t)
	deadOwner := wakeOwner{
		PID:          probe.Process.Pid,
		ProcessStart: live.ProcessStart,
		BootID:       live.BootID,
		SessionID:    live.SessionID,
	}
	if err := validateAuthoritativeWakeOwner(deadOwner); err != nil {
		t.Fatalf("dead owner record invalid: %v", err)
	}

	root, lockPath, targetPath := buildAuthoritativeClaimForRealObserverTest(
		t,
		deadOwner,
		live.ProcessStart,
	)
	beforeLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("dead owner blocked automatic takeover: %v", err)
	}

	afterLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative lock: %v", err)
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative target: %v", err)
	}
	if string(afterLock) != string(beforeLock) ||
		string(afterTarget) != string(beforeTarget) {
		t.Fatal("dead-owner preflight mutated the claim before acquisition")
	}
}

func TestPrepareCoopWakeLockRealObserverLiveOwnerRefuses(t *testing.T) {
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	root, lockPath, targetPath := buildAuthoritativeClaimForRealObserverTest(
		t,
		owner,
		owner.ProcessStart,
	)
	beforeLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	err = prepareCoopWakeLock(root, "codex", true, "unused")
	if err == nil || !strings.Contains(err.Error(), "owned by a live process") {
		t.Fatalf("live owner result = %v, want live-owner refusal", err)
	}
	if strings.Contains(err.Error(), "doctor") ||
		strings.Contains(err.Error(), "wake retire") ||
		!strings.Contains(err.Error(), "pid:") ||
		!strings.Contains(err.Error(), "tty:") ||
		!strings.Contains(err.Error(), "started:") {
		t.Fatalf("live-owner refusal has unsafe or incomplete remedy: %v", err)
	}

	afterLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterLock) != string(beforeLock) ||
		string(afterTarget) != string(beforeTarget) {
		t.Fatal("live-owner refusal mutated the authoritative claim")
	}
}

func TestPrepareCoopWakeLockRealObserverDoesNotBlockOrLeak(t *testing.T) {
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	root, _, _ := buildAuthoritativeClaimForRealObserverTest(
		t,
		owner,
		owner.ProcessStart,
	)

	before := runtime.NumGoroutine()
	start := time.Now()
	const rounds = 100
	for i := 0; i < rounds; i++ {
		if err := prepareCoopWakeLock(root, "codex", true, "unused"); err == nil {
			t.Fatalf("round %d: live owner unexpectedly allowed takeover", i)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 30*time.Second {
		t.Fatalf("%d preflight observations took %s; observation blocks the hot path", rounds, elapsed)
	}

	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf(
			"goroutines grew from %d to %d across %d observed preflights; monitor leak",
			before,
			after,
			rounds,
		)
	}
	t.Logf(
		"%d live-owner preflights in %s, goroutines %d -> %d",
		rounds,
		elapsed,
		before,
		after,
	)
}
