//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const wakeRepairTestBootID = "11111111-1111-1111-1111-111111111111"

type wakeRepairTestStarter func(
	root, me string,
	target wakeTarget,
	floor wakeRepairFloor,
) (int, error)

func stubStartWakeFromTarget(t *testing.T, fn wakeRepairTestStarter) {
	t.Helper()
	old := startWakeFromTarget
	startWakeFromTarget = func(
		agentDir *wakeAgentDir,
		inboxDir *wakeInboxDir,
		root, me string,
		target wakeTarget,
		lineage wakeRepairLineage,
	) (*wakeRepairChild, error) {
		pid, err := fn(root, me, target, lineage.floor)
		if err != nil {
			return nil, err
		}
		winner := inspectWakeLock(root, me)
		floor, exists, err := readWakeRepairFloor(root, me)
		if err != nil {
			return nil, err
		}
		if !exists {
			floor = lineage.floor
		}
		if exists && floor.SourceGeneration == "" && floor.SourceFloorDigest == "" {
			floor.SourceGeneration = lineage.source.DeadGeneration
			floor.SourceFloorDigest = lineage.source.SourceFloorDigest
			if err := writeWakeRepairFloor(root, me, floor); err != nil {
				return nil, err
			}
		}
		if winner.Lock.SourceGeneration == "" && winner.Lock.SourceFloorDigest == "" {
			winner.Lock.SourceGeneration = lineage.source.DeadGeneration
			winner.Lock.SourceFloorDigest = lineage.source.SourceFloorDigest
			data, err := json.Marshal(winner.Lock)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(winner.LockPath, data, 0o600); err != nil {
				return nil, err
			}
			winner = inspectWakeLock(root, me)
		}
		source, err := newWakeRepairHandoffSource(lineage.floor, target, agentDir, inboxDir)
		if err != nil {
			return nil, err
		}
		targetDigest, err := wakeTargetDigest(target)
		if err != nil {
			return nil, err
		}
		floorDigest, err := wakeRepairFloorDigest(floor)
		if err != nil {
			return nil, err
		}
		var floorAuthority wakeRepairFloorAuthority
		if exists {
			err = agentDir.withFD(func(dirfd int) error {
				snapshot, snapshotExists, snapshotErr := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
				if snapshotErr != nil {
					return snapshotErr
				}
				if !snapshotExists {
					return fmt.Errorf("wake repair floor disappeared before test preparation")
				}
				floorAuthority, snapshotErr = newWakeRepairFloorAuthority(snapshot)
				return snapshotErr
			})
			if err != nil {
				return nil, err
			}
		} else {
			floorAuthority = wakeRepairFloorAuthorityForTest(source, winner.Lock.Generation)
		}
		prepared, err := newWakeRepairHandoffPrepared(
			source,
			pid,
			winner.Lock.Generation,
			targetDigest,
			floorDigest,
			floorAuthority,
		)
		if err != nil {
			return nil, err
		}
		return &wakeRepairChild{
			Process:      &os.Process{Pid: pid},
			ProcessStart: winner.Lock.ProcessStart,
			Source:       source,
			Prepared:     prepared,
			admit:        func() error { return nil },
		}, nil
	}
	t.Cleanup(func() {
		startWakeFromTarget = old
	})
}

func ensureWakeRepairLockIdentityForTest(t *testing.T, root, me string) wakeLock {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wake lock: %v", err)
	}
	var lock wakeLock
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("decode wake lock: %v", err)
	}
	changed := false
	if lock.Generation == "" {
		lock.Generation = "test-repair-generation"
		changed = true
	}
	if lock.BootID == "" {
		lock.BootID = wakeRepairTestBootID
		changed = true
	}
	if changed {
		data, err = json.Marshal(lock)
		if err != nil {
			t.Fatalf("marshal wake lock: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("rewrite wake lock: %v", err)
		}
	}
	stubCurrentWakeBootID(t, lock.BootID)
	return lock
}

func writeWakeRepairFloorForTest(
	t *testing.T,
	root, me string,
	target wakeTarget,
	existing map[string]wakeFileIdentity,
) wakeRepairFloor {
	t.Helper()
	lock := ensureWakeRepairLockIdentityForTest(t, root, me)
	floor, err := newWakeRepairFloor(root, me, lock, target, existing)
	if err != nil {
		t.Fatalf("newWakeRepairFloor: %v", err)
	}
	if err := writeWakeRepairFloor(root, me, floor); err != nil {
		t.Fatalf("writeWakeRepairFloor: %v", err)
	}
	return floor
}

func writeWakeRepairWinnerFloorForTest(
	t *testing.T,
	root, me string,
	target wakeTarget,
	source wakeRepairFloor,
) {
	t.Helper()
	lock := ensureWakeRepairLockIdentityForTest(t, root, me)
	floor, err := newWakeRepairFloor(root, me, lock, target, source.Existing)
	if err != nil {
		t.Fatalf("new winner wake repair floor: %v", err)
	}
	if err := writeWakeRepairFloor(root, me, floor); err != nil {
		t.Fatalf("write winner wake repair floor: %v", err)
	}
}

func TestWakeTargetWriteReadRoundTripAndPermissions(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "target"})

	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	info, err := os.Stat(wakeTargetPath(root, "codex"))
	if err != nil {
		t.Fatalf("stat wake target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("wake target mode = %o, want 0600", got)
	}
	got, exists, err := readWakeTarget(root, "codex")
	if err != nil {
		t.Fatalf("readWakeTarget: %v", err)
	}
	if !exists {
		t.Fatal("expected wake target to exist")
	}
	if got.Mode != wakeTargetInjectVia || got.InjectVia != injector {
		t.Fatalf("unexpected target: %#v", got)
	}
	if strings.Join(got.InjectArgs, "|") != "exec|target" {
		t.Fatalf("inject args = %#v", got.InjectArgs)
	}
}

func TestWakeTargetRejectsWorldWritableInjectVia(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	_, err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected world-writable rejection, got %v", err)
	}
}

func TestReadWakeTargetRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "codex", mustNewWakeTargetForTest(t, root, "codex", injector, nil)); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	targetPath := wakeTargetPath(root, "codex")
	symlinkTarget := targetPath + ".other"
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if err := os.WriteFile(symlinkTarget, data, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Remove(targetPath); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	if err := os.Symlink(symlinkTarget, targetPath); err != nil {
		t.Fatalf("symlink target: %v", err)
	}

	_, exists, err := readWakeTarget(root, "codex")
	if !exists {
		t.Fatal("expected symlink target to be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestRepairWakeRefusesTamperedTargetDigest(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	tampered := target
	tampered.InjectArgs = []string{"evil"}
	if err := writeWakeTarget(root, "codex", tampered); err != nil {
		t.Fatalf("write tampered wake target: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget, _ wakeRepairFloor) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for tampered target")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected digest mismatch refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "does not match") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain on refused tampered target: %v", statErr)
	}
}

func TestRepairWakeSupersedesUnverifiedGenericLockWithoutSignal(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
		Generation: "unverified-generation",
		BootID:     wakeRepairTestBootID,
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == 4242 {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		if pid == 9876 {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				BootID:     wakeRepairTestBootID,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeRepairFloorForTest(t, root, "codex", target, nil)
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, gotTarget wakeTarget, source wakeRepairFloor) (int, error) {
		if gotRoot != root || gotMe != "codex" || !sameWakeTarget(gotTarget, target) {
			t.Fatalf("unexpected repair start: root=%q me=%q target=%#v", gotRoot, gotMe, gotTarget)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("unverified lock should be removed before start: %v", err)
		}
		writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
			PID:          9876,
			ProcessStart: "new-start",
			Executable:   "/opt/homebrew/bin/amq",
			Generation:   "generation-new",
		}, target))
		writeWakeRepairWinnerFloorForTest(t, root, "codex", target, source)
		return 9876, nil
	})

	var result wakeRepairResult
	var repairErr error
	stderr := captureWakeStderr(t, func() {
		result, repairErr = repairWake(root, "codex")
	})
	if repairErr != nil {
		t.Fatalf("repairWake: %v", repairErr)
	}
	if result.Status != "repaired" || result.PID != 9876 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(signals) != 0 {
		t.Fatalf("unverified helper was signaled: %v", signals)
	}
	if count := strings.Count(stderr, "warning:"); count != 1 {
		t.Fatalf("warning count = %d, want 1:\n%s", count, stderr)
	}
	for _, want := range []string{
		"unidentified wake helper",
		"pid 4242",
		"duplicate notifications",
		"stop that helper if duplicates persist",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr)
		}
	}
}

func TestRunWakeRepairCLIRepairsStaleWakeWithJSON(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == 9876 {
			return wakeProcessInfo{PID: pid, Running: true, StartToken: "new-start", BootID: wakeRepairTestBootID, Executable: "/opt/homebrew/bin/amq", Args: []string{"amq", "wake", "--root", root, "--me", "codex"}}
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeRepairFloorForTest(t, root, "codex", target, nil)
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, target wakeTarget, source wakeRepairFloor) (int, error) {
		if gotRoot != root || gotMe != "codex" {
			t.Fatalf("start root/me = %q/%q", gotRoot, gotMe)
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("lock should be removed before start, stat=%v", err)
		}
		writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
			PID: 9876, ProcessStart: "new-start", Executable: "/opt/homebrew/bin/amq", Generation: "generation-new",
		}, target))
		writeWakeRepairWinnerFloorForTest(t, root, "codex", target, source)
		return 9876, nil
	})

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "codex", "--json"})
	})
	if runErr != nil {
		t.Fatalf("runWakeRepair: %v", runErr)
	}

	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "repaired" || result.PID != 9876 || !result.RepairAvailable {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func captureWakeRepairOutput(t *testing.T, fn func() error) (stdout, stderr string, runErr error) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr

	defer func() {
		_ = wOut.Close()
		_ = wErr.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	runErr = fn()
	_ = wOut.Close()
	_ = wErr.Close()
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	_ = rOut.Close()
	_ = rErr.Close()
	return string(outBytes), string(errBytes), runErr
}
