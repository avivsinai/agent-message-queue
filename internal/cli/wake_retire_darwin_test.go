//go:build darwin

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRetireWakeUsesDarwinCooperativeControlAndRemovesTarget(t *testing.T) {
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
	if _, err := os.Stat(wakeTargetPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("retired target still exists: %v", err)
	}
}

func TestRetireWakeDarwinUsesRetainedDirectoryAfterAgentReplacement(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, _ := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
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
	detachedPath, successorLock := installRetireAgentDirectoryReplacement(
		t, root, "codex", ".wake.lock", wakeTargetFileName,
	)

	result, err := retireWake(root, "codex", requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, ".wake.lock")); !os.IsNotExist(err) {
		t.Fatalf("retained exact lock survived retirement: %v", err)
	}
	assertRetireReplacementLockPreserved(t, root, "codex", successorLock())
}

func TestRetireWakeDarwinPreservesLiveBoundStateReplacement(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := inspectWakeLock(fixture.root, fixture.me).Lock
	lock.Executable = "/opt/homebrew/bin/amq"
	lock.Args = []string{"/opt/homebrew/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", requested.InjectVia}
	lock.ControlSocket = wakeControlSocketPath(fixture.root, fixture.me, lock.Generation)
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcessFromLock(pid, lock)
	})
	cleanup, stopRequested, markStopped, err := startWakeControlListener(fixture.root, fixture.me, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() {
		<-stopRequested
		markStopped()
	}()
	statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
	stateRaw, installReplacement := stageExactWakeArtifactReplacement(t, statePath, ".live-replacement")
	originalHook := afterWakeRetireLockRemoval
	afterWakeRetireLockRemoval = func() {
		if err := installReplacement(); err != nil {
			t.Errorf("install live bound state replacement: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeRetireLockRemoval = originalHook })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target removed with live replacement state: %v", err)
	}
	assertFileRawForTest(t, statePath, stateRaw)
}

func TestRetireWakeDarwinDetachedBoundStateValidationUsesRetainedDirectory(t *testing.T) {
	t.Run("missing successor state during authentication", func(t *testing.T) {
		fixture, requested, cleanup, stopped, markStopped := setupBoundDarwinRetire(t)
		defer cleanup()
		detachedPath, successorLock := installRetireAgentDirectoryReplacement(
			t, fixture.root, fixture.me, ".wake.lock", wakeTargetFileName,
		)
		go func() {
			<-stopped
			markStopped()
		}()

		result, err := retireWake(fixture.root, fixture.me, requested)
		if err != nil || result.Status != "retired_with_residue" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
		assertRetireReplacementLockPreserved(t, fixture.root, fixture.me, successorLock())
		assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName))
	})

	t.Run("divergent successor state during completion", func(t *testing.T) {
		fixture, requested, cleanup, stopped, markStopped := setupBoundDarwinRetire(t)
		defer cleanup()
		detachedPath, successorLock := installRetireAgentDirectoryReplacement(
			t,
			fixture.root,
			fixture.me,
			".wake.lock",
			wakeTargetFileName,
			wakeStateFileName,
		)
		go func() {
			<-stopped
			statePath := filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Errorf("read successor state: %v", err)
				markStopped()
				return
			}
			state, err := decodeWakeState(raw)
			if err != nil {
				t.Errorf("decode successor state: %v", err)
				markStopped()
				return
			}
			state.Target.TargetDigest = "sha256:" + strings.Repeat("0", 64)
			divergent, err := json.Marshal(state)
			if err != nil {
				t.Errorf("marshal divergent successor state: %v", err)
				markStopped()
				return
			}
			if err := os.WriteFile(statePath, divergent, 0o600); err != nil {
				t.Errorf("write divergent successor state: %v", err)
			}
			markStopped()
		}()

		result, err := retireWake(fixture.root, fixture.me, requested)
		if err != nil || result.Status != "retired_with_residue" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
		assertRetireReplacementLockPreserved(t, fixture.root, fixture.me, successorLock())
	})
}

func TestRetireWakeDarwinLockFsyncFailureACKsCommittedAndSkipsCleanup(t *testing.T) {
	fixture, requested, cleanup, stopped, markStopped := setupBoundDarwinRetire(t)
	defer cleanup()
	go func() {
		<-stopped
		markStopped()
	}()
	originalSync := syncWakeLockAfterCommitDirFD
	syncWakeLockAfterCommitDirFD = func(int) error { return syscall.EIO }
	t.Cleanup(func() { syncWakeLockAfterCommitDirFD = originalSync })

	result, err := retireWake(fixture.root, fixture.me, requested)
	if err != nil || result.Status != "retired_with_residue" ||
		!strings.Contains(result.Reason, "wake lock durability") ||
		!strings.Contains(result.Reason, "next acquisition") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertPathMissingForTest(t, filepath.Join(fsq.AgentBase(fixture.root, fixture.me), ".wake.lock"))
	if _, err := os.Stat(wakeTargetPath(fixture.root, fixture.me)); err != nil {
		t.Fatalf("target cleanup was not skipped after listener fsync failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(fixture.root, fixture.me), wakeStateFileName)); err != nil {
		t.Fatalf("state cleanup was not skipped after listener fsync failure: %v", err)
	}
}

func TestRunWakeRetireDarwinACKReportsDetachedAndDurabilityResidue(t *testing.T) {
	for _, tc := range []struct {
		name string
		json bool
	}{
		{name: "json", json: true},
		{name: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, requested, cleanup, stopped, markStopped := setupBoundDarwinRetire(t)
			defer cleanup()
			go func() {
				<-stopped
				markStopped()
			}()
			detachedPath, successorLock := installRetireAgentDirectoryReplacement(
				t, fixture.root, fixture.me, ".wake.lock", wakeTargetFileName,
			)
			originalSync := syncWakeLockAfterCommitDirFD
			syncWakeLockAfterCommitDirFD = func(int) error { return syscall.EIO }
			t.Cleanup(func() { syncWakeLockAfterCommitDirFD = originalSync })

			args := []string{"retire", "--root", fixture.root, "--me", fixture.me, "--inject-via", requested.InjectVia}
			for _, arg := range requested.InjectArgs {
				args = append(args, "--inject-arg", arg)
			}
			if tc.json {
				args = append(args, "--json")
			}
			stdout, _, runErr := captureWakeRepairOutput(t, func() error { return runWake(args) })
			if runErr != nil {
				t.Fatalf("runWake returned post-ACK refusal: %v\nstdout=%s", runErr, stdout)
			}
			if tc.json {
				var result wakeRetireResult
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatalf("unmarshal JSON: %v\nstdout=%s", err, stdout)
				}
				if result.Status != "retired_with_residue" ||
					!strings.Contains(result.Reason, "detached wake cleanup") ||
					!strings.Contains(result.Reason, "wake lock durability") ||
					!strings.Contains(result.Reason, "next acquisition") {
					t.Fatalf("result=%#v", result)
				}
			} else if !strings.Contains(stdout, "wake retire: retired_with_residue") ||
				!strings.Contains(stdout, "detached wake cleanup") ||
				!strings.Contains(stdout, "wake lock durability") ||
				!strings.Contains(stdout, "next acquisition") {
				t.Fatalf("stdout=%q", stdout)
			}
			assertPathMissingForTest(t, filepath.Join(detachedPath, ".wake.lock"))
			assertRetireReplacementLockPreserved(t, fixture.root, fixture.me, successorLock())
		})
	}
}

func TestRunWakeRetireDarwinACKCommitReplacementExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		json bool
	}{
		{name: "json", json: true},
		{name: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const wakePID = 4242
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			requested, _ := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
			lock := inspectWakeLock(root, "codex").Lock
			lock.ControlSocket = wakeControlSocketPath(root, "codex", lock.Generation)
			writeWakeLockForTest(t, root, "codex", lock)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return matchingRetireWakeProcessFromLock(pid, lock)
			})
			cleanup, stopped, markStopped, err := startWakeControlListener(root, "codex", lock)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			go func() {
				<-stopped
				markStopped()
			}()
			originalSync := syncWakeLockAfterCommitDirFD
			installedReplacement := false
			syncWakeLockAfterCommitDirFD = func(int) error {
				if !installedReplacement {
					installedReplacement = true
					writeWakeLockForTest(t, root, "codex", lock)
				}
				return nil
			}
			t.Cleanup(func() { syncWakeLockAfterCommitDirFD = originalSync })

			args := []string{"retire", "--root", root, "--me", "codex", "--inject-via", requested.InjectVia}
			for _, arg := range requested.InjectArgs {
				args = append(args, "--inject-arg", arg)
			}
			if tc.json {
				args = append(args, "--json")
			}
			stdout, _, runErr := captureWakeRepairOutput(t, func() error { return runWake(args) })
			if runErr != nil {
				t.Fatalf("runWake returned post-ACK refusal: %v\nstdout=%s", runErr, stdout)
			}
			if tc.json {
				var result wakeRetireResult
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatalf("unmarshal JSON: %v\nstdout=%s", err, stdout)
				}
				if result.Status != "retired_with_residue" ||
					!strings.Contains(result.Reason, "replacement wake lock") ||
					strings.Contains(result.Reason, "wake lock durability") ||
					!strings.Contains(result.Reason, "next acquisition") {
					t.Fatalf("result=%#v", result)
				}
			} else if !strings.Contains(stdout, "wake retire: retired_with_residue") ||
				!strings.Contains(stdout, "replacement wake lock") ||
				strings.Contains(stdout, "wake lock durability") ||
				!strings.Contains(stdout, "next acquisition") {
				t.Fatalf("stdout=%q", stdout)
			}
			if !installedReplacement {
				t.Fatal("listener did not reach exact-unlink durability seam")
			}
			if _, err := os.Stat(wakeTargetPath(root, "codex")); err != nil {
				t.Fatalf("target cleanup was not skipped after post-ACK replacement: %v", err)
			}
		})
	}
}

func setupBoundDarwinRetire(
	t *testing.T,
) (*genericWakePreparedCleanupFixture, wakeTarget, func(), <-chan struct{}, func()) {
	t.Helper()
	fixture := newGenericWakePreparedCleanupFixture(t, true)
	requested := *fixture.target
	lock := inspectWakeLock(fixture.root, fixture.me).Lock
	lock.Executable = "/opt/homebrew/bin/amq"
	lock.Args = []string{"/opt/homebrew/bin/amq", "wake", "--root", fixture.root, "--me", fixture.me, "--inject-via", requested.InjectVia}
	lock.ControlSocket = wakeControlSocketPath(fixture.root, fixture.me, lock.Generation)
	writeWakeLockForTest(t, fixture.root, fixture.me, lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcessFromLock(pid, lock)
	})
	cleanup, stopped, markStopped, err := startWakeControlListener(fixture.root, fixture.me, lock)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, requested, cleanup, stopped, markStopped
}
