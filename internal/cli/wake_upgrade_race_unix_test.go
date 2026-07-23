//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeUpgradeRaceRepairRejectsConcurrentCurrentAcquire(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-a"})
	owner := wakeOwner{PID: 4343, ProcessStart: "owner-start", BootID: "boot-1"}
	target.Owner = &owner
	legacyGeneration := "legacy-without-control-metadata"
	writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
		Generation: legacyGeneration,
	}, target))
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write legacy wake target: %v", err)
	}

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case os.Getpid():
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "current-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
			}
		case owner.PID:
			return wakeProcessInfoForOwnerTest(owner)
		default:
			return wakeProcessInfo{PID: pid, Running: false}
		}
	})
	repairStarted := make(chan struct{})
	releaseRepair := make(chan struct{})
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, gotTarget wakeTarget) (int, error) {
		if gotRoot != root || gotMe != "codex" || !sameWakeInjectorIdentity(gotTarget, target) {
			t.Fatalf("repair starter got root=%q me=%q target=%#v", gotRoot, gotMe, gotTarget)
		}
		close(repairStarted)
		<-releaseRepair
		return 0, errors.New("lost start race")
	})

	type repairResult struct {
		result wakeRepairResult
		err    error
	}
	repaired := make(chan repairResult, 1)
	go func() {
		result, err := repairWake(root, "codex")
		repaired <- repairResult{result: result, err: err}
	}()
	<-repairStarted // The legacy stale generation has been removed and the guard released.

	cleanup, err := acquireWakeLock(root, "codex", &target)
	if err != nil {
		t.Fatalf("concurrent current-version acquire: %v", err)
	}
	defer cleanup()
	winner := inspectWakeLock(root, "codex")
	if winner.Status != wakeLockValid || !winner.IdentityConfirmed {
		t.Fatalf("concurrent winner = %#v, want confirmed valid", winner)
	}
	if winner.Lock.Generation == "" || winner.Lock.Generation == legacyGeneration {
		t.Fatalf("winner generation = %q, want a new current generation", winner.Lock.Generation)
	}
	if winner.Lock.WakeMode != wakeTargetInjectVia || winner.Lock.TargetDigest != wakeTargetDigest(target) {
		t.Fatalf("winner metadata = %#v, want current inject-via target binding", winner.Lock)
	}
	if runtime.GOOS == "darwin" && winner.Lock.ControlSocket == "" {
		t.Fatal("Darwin current-version winner omitted cooperative control metadata")
	}

	close(releaseRepair)
	got := <-repaired
	if got.err == nil || !strings.Contains(got.err.Error(), "lost start race") {
		t.Fatalf("repair accepted a winner that did not install its baseline: result=%#v err=%v", got.result, got.err)
	}
	if got.result.Status != "error" {
		t.Fatalf("repair result = %#v, want rejected concurrent winner", got.result)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeInjectorIdentity(persisted, target) {
		t.Fatalf("persisted target = (%#v,%v,%v), want exact current target", persisted, exists, err)
	}
}

func TestWakeUpgradeRaceCleanupAndReadinessPreserveReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	originalTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-a"})
	replacementTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-b"})
	cleanup, err := acquireWakeLock(root, "codex", &originalTarget)
	if err != nil {
		t.Fatalf("acquire original wake: %v", err)
	}
	original := inspectWakeLock(root, "codex")
	readyPath := fsq.AgentBase(root, "codex") + "/wake-upgrade.ready"

	holderEntered := make(chan struct{})
	replaceNow := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(holderEntered)
			<-replaceNow
			if err := writeWakeTargetGuarded(root, "codex", replacementTarget); err != nil {
				return err
			}
			replacement := original.Lock
			replacement.Generation = "replacement-generation"
			replacement.TargetDigest = wakeTargetDigest(replacementTarget)
			replacement.ControlSocket = wakeControlSocketPath(root, "codex", replacement.Generation)
			data, err := json.Marshal(replacement)
			if err != nil {
				return err
			}
			if err := os.Remove(original.LockPath); err != nil {
				return err
			}
			return os.WriteFile(original.LockPath, data, 0o600)
		})
	}()
	<-holderEntered

	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan struct{})
	go func() {
		close(cleanupStarted)
		cleanup()
		close(cleanupDone)
	}()
	readyStarted := make(chan struct{})
	readyDone := make(chan error, 1)
	go func() {
		close(readyStarted)
		readyDone <- writeWakeReadyFile(root, "codex", readyPath, original)
	}()
	<-cleanupStarted
	<-readyStarted
	close(replaceNow)
	if err := <-holderDone; err != nil {
		t.Fatalf("replace under lifecycle guard: %v", err)
	}
	<-cleanupDone
	readyErr := <-readyDone
	if readyErr == nil || !strings.Contains(readyErr.Error(), "generation changed") {
		t.Fatalf("old readiness error = %v, want generation-change refusal", readyErr)
	}
	if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
		t.Fatalf("old generation published readiness: %v", err)
	}

	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != "replacement-generation" {
		t.Fatalf("old cleanup removed replacement: %#v", current)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeInjectorIdentity(persisted, replacementTarget) {
		t.Fatalf("replacement target = (%#v,%v,%v), want exact replacement", persisted, exists, err)
	}
}

func TestWakeUpgradeRaceRetireRefusesLockAndTargetReplacement(t *testing.T) {
	const oldPID = 4242
	const replacementPID = 4343
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, oldPID)
	replacementTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-b"})

	initialInspected := make(chan struct{})
	var initialOnce sync.Once
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == oldPID {
			initialOnce.Do(func() { close(initialInspected) })
		}
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	holderEntered := make(chan struct{})
	replaceNow := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(holderEntered)
			<-replaceNow
			if err := writeWakeTargetGuarded(root, "codex", replacementTarget); err != nil {
				return err
			}
			replacement := bindWakeLockToTarget(wakeLock{
				PID:          replacementPID,
				TTY:          "unknown",
				ProcessStart: "wake-start",
				BootID:       "boot-1",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
				Generation:   "replacement-generation",
			}, replacementTarget)
			replacement.ControlSocket = wakeControlSocketPath(root, "codex", replacement.Generation)
			data, err := json.Marshal(replacement)
			if err != nil {
				return err
			}
			if err := os.Remove(lockPath); err != nil {
				return err
			}
			return os.WriteFile(lockPath, data, 0o600)
		})
	}()
	<-holderEntered

	type retireResult struct {
		result wakeRetireResult
		err    error
	}
	retired := make(chan retireResult, 1)
	go func() {
		result, err := retireWake(root, "codex", requested)
		retired <- retireResult{result: result, err: err}
	}()
	<-initialInspected // retire captured the old generation before waiting on the guard.
	close(replaceNow)
	if err := <-holderDone; err != nil {
		t.Fatalf("replace under lifecycle guard: %v", err)
	}
	got := <-retired
	if got.err == nil || got.result.Status != "refused" || !strings.Contains(got.result.Reason, "changed before retirement") {
		t.Fatalf("retire result = %#v err=%v, want replacement refusal", got.result, got.err)
	}

	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.PID != replacementPID || current.Lock.Generation != "replacement-generation" {
		t.Fatalf("retire changed replacement lock: %#v", current)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeInjectorIdentity(persisted, replacementTarget) {
		t.Fatalf("retire changed replacement target: (%#v,%v,%v)", persisted, exists, err)
	}
}
