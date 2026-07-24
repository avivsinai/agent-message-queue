//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunOpsChecksReportsWakeRepairAvailabilityWithFloor(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}
	injector := writeExecutableForTest(t, "doctor-repair-injector")
	target := mustNewWakeTargetForTest(t, root, "alice", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "alice", bindWakeLockToTarget(wakeLock{
		PID:        999999999,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "alice", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	writeWakeRepairFloorForTest(t, root, "alice", target, nil)

	result := runOpsChecks(root, "test_source", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) || !got.TargetPresent || got.TargetReason != "" {
		t.Fatalf("unexpected stale repair state: %#v", got)
	}
	if !got.RepairAvailable || got.Repair != wakeRepairCommand(root, "alice") || got.RepairReason != "" {
		t.Fatalf("repair availability = %#v", got)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("doctor report removed lock: %v", err)
	}

	stubCurrentWakeBootID(t, "22222222-2222-2222-2222-222222222222")
	result = runOpsChecks(root, "test_source", false)
	got = result.WakeLocks[0]
	if got.RepairAvailable || got.Repair != "" ||
		!strings.Contains(got.RepairReason, "does not match the current boot") {
		t.Fatalf("doctor advertised prior-boot repair floor: %#v", got)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("doctor prior-boot report removed lock: %v", err)
	}
}

func TestRunOpsChecksRejectsSymlinkAndFIFOWakeLocks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, path string)
		wantError string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "lock.json")
				if err := os.WriteFile(target, []byte(`{"pid":4242}`), 0o600); err != nil {
					t.Fatalf("write target lock: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink lock: %v", err)
				}
			},
			wantError: "must not be a symlink",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo lock: %v", err)
				}
			},
			wantError: "must be a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentBase := fsq.AgentBase(root, "codex")
			if err := os.MkdirAll(agentBase, 0o700); err != nil {
				t.Fatalf("mkdir agent base: %v", err)
			}
			tc.setup(t, filepath.Join(agentBase, ".wake.lock"))

			done := make(chan *doctorOpsResult, 1)
			go func() {
				done <- runOpsChecks(root, "test", false)
			}()

			select {
			case result := <-done:
				if len(result.WakeLocks) != 1 {
					t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
				}
				got := result.WakeLocks[0]
				if got.Status != string(wakeLockUnverified) || !strings.Contains(got.Reason, tc.wantError) {
					t.Fatalf("unexpected wake lock: %#v", got)
				}
				if got.RepairAvailable || got.Repair != "" {
					t.Fatalf("repair advertised for unsafe lock: %#v", got)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("doctor ops blocked on wake lock")
			}
		})
	}
}

func TestRunOpsChecksLeavesForeignAgentSymlinkWakeLockUntouched(t *testing.T) {
	root := secureTempDirForTest(t)
	foreign := secureTempDirForTest(t)
	foreignAgent := fsq.AgentBase(foreign, "codex")
	if err := os.MkdirAll(foreignAgent, 0o700); err != nil {
		t.Fatalf("mkdir foreign agent: %v", err)
	}
	lockPath := filepath.Join(foreignAgent, ".wake.lock")
	if err := os.WriteFile(lockPath, []byte(`{"pid":4242}`), 0o600); err != nil {
		t.Fatalf("write foreign lock: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(fsq.AgentBase(root, "codex")), 0o700); err != nil {
		t.Fatalf("mkdir agents root: %v", err)
	}
	if err := os.Symlink(foreignAgent, fsq.AgentBase(root, "codex")); err != nil {
		t.Fatalf("symlink foreign agent: %v", err)
	}
	result := runOpsChecks(root, "test", true)
	if len(result.WakeLocks) != 0 {
		t.Fatalf("foreign agent symlink should not be inspected: %#v", result.WakeLocks)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("foreign wake lock was touched: %v", err)
	}
}

func TestRunOpsChecksRejectsFIFOWakeTargetWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "codex")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	targetPath := wakeTargetPath(root, "codex")
	if err := syscall.Mkfifo(targetPath, 0o600); err != nil {
		t.Fatalf("mkfifo wake target: %v", err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
		TargetDigest: "sha256:fake",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	done := make(chan *doctorOpsResult, 1)
	go func() {
		done <- runOpsChecks(root, "test", false)
	}()

	select {
	case result := <-done:
		if len(result.WakeLocks) != 1 {
			t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
		}
		got := result.WakeLocks[0]
		if !got.TargetPresent || !strings.Contains(got.TargetReason, "must be a regular file") {
			t.Fatalf("unexpected target fields: %#v", got)
		}
		if got.RepairAvailable || got.Repair != "" {
			t.Fatalf("repair advertised for unsafe target: %#v", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("doctor ops blocked on wake target")
	}
}

func TestRunOpsChecksDoesNotAdvertiseRepairForTamperedStaleLock(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	writeWakeLockExactForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:   4242,
		Root:  filepath.Join(root, "other-root"),
		Agent: "codex",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result := runOpsChecks(root, "test", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) || got.Reason != "root mismatch" {
		t.Fatalf("unexpected wake lock: %#v", got)
	}
	if got.RepairAvailable || got.Repair != "" {
		t.Fatalf("repair advertised for tampered stale lock: %#v", got)
	}
}

func TestRunOpsChecksDirectsOwnerClaimToRecoverOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "owner-doctor-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Generation:   "owner-doctor-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockExactForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	result := runOpsChecks(root, "test", true)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) {
		t.Fatalf("owner wake status = %q, want stale", got.Status)
	}
	if got.Fix != wakeRecoverOwnerCommand(root, "codex") {
		t.Fatalf("owner wake fix = %q, want %q", got.Fix, wakeRecoverOwnerCommand(root, "codex"))
	}
	if got.Removed {
		t.Fatal("doctor --fix removed an owner-bound wake claim")
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("owner-bound wake claim was not preserved: %v", err)
	}
}

func TestRunOpsChecksDoesNotAdvertiseRepairForUnknownBootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lock       wakeLock
		process    wakeProcessInfo
		wantReason string
	}{
		{
			name: "boot id mismatch",
			lock: wakeLock{
				PID:          4242,
				ProcessStart: "start-token",
				BootID:       "recorded-boot",
				Executable:   "/opt/homebrew/bin/amq",
			},
			process: wakeProcessInfo{
				PID:        4242,
				Running:    true,
				StartToken: "start-token",
				BootID:     "actual-boot",
				Executable: "/opt/homebrew/bin/amq",
			},
			wantReason: "boot id mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatalf("write wake target: %v", err)
			}
			writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(tc.lock, target))
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				proc := tc.process
				proc.PID = pid
				proc.Args = []string{"amq", "wake", "--root", root, "--me", "codex"}
				return proc
			})

			result := runOpsChecks(root, "test", false)
			if len(result.WakeLocks) != 1 {
				t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
			}
			got := result.WakeLocks[0]
			if got.Status != string(wakeLockUnverified) || got.Reason != tc.wantReason {
				t.Fatalf("unexpected wake lock: %#v", got)
			}
			if !got.TargetPresent || got.TargetReason != "" {
				t.Fatalf("unexpected target fields: %#v", got)
			}
			if got.RepairAvailable || got.Repair != "" {
				t.Fatalf("repair advertised for live identity mismatch: %#v", got)
			}
		})
	}
}

func TestRunOpsChecksFixRefusesUnknownBootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lock       wakeLock
		process    wakeProcessInfo
		wantReason string
	}{
		{
			name: "boot id mismatch",
			lock: wakeLock{
				PID:          4242,
				ProcessStart: "start-token",
				BootID:       "recorded-boot",
				Executable:   "/opt/homebrew/bin/amq",
			},
			process: wakeProcessInfo{
				PID:        4242,
				Running:    true,
				StartToken: "start-token",
				BootID:     "actual-boot",
				Executable: "/opt/homebrew/bin/amq",
			},
			wantReason: "boot id mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatalf("write wake target: %v", err)
			}
			lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(tc.lock, target))
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				proc := tc.process
				proc.PID = pid
				proc.Args = []string{"amq", "wake", "--root", root, "--me", "codex"}
				return proc
			})

			result := runOpsChecks(root, "test", true)
			if len(result.WakeLocks) != 1 {
				t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
			}
			got := result.WakeLocks[0]
			if got.Status != string(wakeLockUnverified) || got.Reason != tc.wantReason {
				t.Fatalf("unexpected wake lock fix result: %#v", got)
			}
			if got.Removed {
				t.Fatalf("identity-mismatch lock should not be marked removed: %#v", got)
			}
			if got.RepairAvailable || got.Repair != "" {
				t.Fatalf("repair should be cleared after refused fix: %#v", got)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("identity-mismatch lock should remain after fix refusal: %v", statErr)
			}
		})
	}
}

func TestDoctorFixWaitsForWakeLifecycleGuard(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID: 4242, Executable: "/opt/homebrew/bin/amq", Generation: "stale-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	fixed := make(chan []opsWakeLock, 1)
	go func() { fixed <- checkWakeLocks(root, []string{"codex"}, true) }()
	time.Sleep(25 * time.Millisecond)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("doctor fix removed lock before lifecycle guard release: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("guard holder: %v", err)
	}
	locks := <-fixed
	if len(locks) != 1 || locks[0].Status != "fixed" || !locks[0].Removed {
		t.Fatalf("unexpected doctor fix result: %#v", locks)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("doctor fix did not remove stale generation: %v", err)
	}
	if _, err := os.Stat(wakeLifecycleGuardPath(root, "codex")); err != nil {
		t.Fatalf("doctor fix removed permanent lifecycle guard: %v", err)
	}
}

func TestRunOpsChecksReportsProvenStartMismatchAsStale(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "recorded-start",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "actual-start",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	result := runOpsChecks(root, "test", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) || got.Reason != "process start time mismatch" {
		t.Fatalf("unexpected wake lock: %#v", got)
	}
}

func TestRunOpsChecksFixRemovesProvenStartMismatch(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "recorded-start",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "actual-start",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	result := runOpsChecks(root, "test", true)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != "fixed" || !got.Removed {
		t.Fatalf("unexpected wake lock fix result: %#v", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("proven stale lock still exists: %v", err)
	}
}
