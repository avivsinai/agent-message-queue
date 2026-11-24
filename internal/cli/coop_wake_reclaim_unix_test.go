//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestPrepareCoopWakeLockRemovesProvenStaleWithoutPrompt(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, Generation: "stale"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })

	if err := prepareCoopWakeLock(root, "codex", false, "unused"); err != nil {
		t.Fatalf("prepare stale wake lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock remains: %v", err)
	}
}

func TestPrepareCoopWakeLockLiveAuthoritativeRefusesWithoutMutation(t *testing.T) {
	const wakePID = 66121
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "authoritative-coop-conflict-injector")
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
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
		ProcessStart: owner.ProcessStart,
		BootID:       owner.BootID,
		Executable:   wakeArgs[0],
		Args:         wakeArgs,
		Generation:   "authoritative-conflict",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockExactForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	targetPath := wakeTargetPath(root, "codex")
	beforeLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	processRunning := true
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    processRunning,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
			Executable: wakeArgs[0],
			Args:       wakeArgs,
		}
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("authoritative wake must not be signaled through coop startup")
		return nil
	})
	ownerState := wakeOwnerSame
	var observeErr error
	observationClosed := false
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if !sameWakeOwner(&got, &owner) {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		if observeErr != nil {
			monitor := newWakeOwnerObservationMonitor(func() error {
				observationClosed = true
				return nil
			})
			monitor.finish(nil)
			return wakeOwnerObservation{State: wakeOwnerUnknown, monitor: monitor}, observeErr
		}
		return wakeOwnerObservation{State: ownerState}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	err = prepareCoopWakeLock(root, "codex", true, "unused")
	if err == nil || !strings.Contains(err.Error(), "owned by a live process") {
		t.Fatalf("prepare live authoritative wake = %v, want live-owner refusal", err)
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
		t.Fatalf("authoritative lock changed: %v", err)
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("authoritative target changed: %v", err)
	}
	if string(afterLock) != string(beforeLock) || string(afterTarget) != string(beforeTarget) {
		t.Fatal("authoritative claim changed during startup refusal")
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wakeOwnerLockFileMode {
		t.Fatalf("authoritative lock mode = %o, want %o", got, wakeOwnerLockFileMode)
	}

	ownerState = wakeOwnerDead
	processRunning = false
	err = prepareCoopWakeLock(root, "codex", true, "unused")
	wantRecovery := wakeRecoverOwnerCommand(root, "codex")
	if err == nil || !strings.Contains(err.Error(), wantRecovery) ||
		!strings.Contains(err.Error(), "then retry") ||
		strings.Contains(err.Error(), "'amq wake recover-owner --me codex'") {
		t.Fatalf("stale authoritative wake result = %v, want actionable %q remedy", err, wantRecovery)
	}
	processRunning = true
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("dead-owner authoritative wake blocked automatic takeover: %v", err)
	}
	afterDeadOwnerLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative lock: %v", err)
	}
	afterDeadOwnerTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative target: %v", err)
	}
	if string(afterDeadOwnerLock) != string(beforeLock) ||
		string(afterDeadOwnerTarget) != string(beforeTarget) {
		t.Fatal("dead-owner preflight mutated authoritative claim before acquisition")
	}

	observeErr = errors.New("owner observer failed")
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err == nil {
		t.Fatal("owner observation failure did not block startup")
	}
	if !observationClosed {
		t.Fatal("owner observation failure leaked its returned capability")
	}
}

// Field report 2026-11-24: after a machine restart, every pre-restart wake
// lock was proven stale (boot id mismatch) while the restart gave its PID to
// an unrelated live process. prepareCoopWakeLock refused coop exec on exactly
// those locks. A proven-stale lock belongs to no running process; the exact
// removal sends no signal, so startup must take the stale branch first.
func TestPrepareCoopWakeLockRemovesProvenStaleLockWhosePIDIsALiveForeignProcess(t *testing.T) {
	const reusedPID = 99999
	root := secureTempDirForTest(t)
	lock := wakeLock{
		PID:        reusedPID,
		Generation: "restart-stale",
		BootID:     "00000000-0000-0000-0000-00000000000" + "1",
	}
	// The recorded boot id must be a UUID that is not this boot, so the lock
	// is proven stale while its PID is live.
	if current := currentWakeBootID(); launch.IsBootUUID(current) &&
		launch.CompareBootID(lock.BootID, current) != launch.BootIDMismatch {
		lock.BootID = "ffffffff-0000-0000-0000-00000000000" + "1"
		if launch.CompareBootID(lock.BootID, current) != launch.BootIDMismatch {
			t.Skipf("no provably foreign boot uuid available (current %q)", current)
		}
	}
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		// The live process at the reused PID is this test itself: running and
		// affirmatively proven not to be an amq wake.
		return wakeProcessInfo{PID: pid, Running: true, Executable: "go", Args: []string{"go", "test"}}
	})

	if err := prepareCoopWakeLock(root, "codex", false, "unused"); err != nil {
		t.Fatalf("prepare proven-stale lock with a live reused pid: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("proven-stale lock remains after startup: %v", err)
	}
}
