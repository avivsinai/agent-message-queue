//go:build darwin || linux

package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func stubStartWakeFromTarget(t *testing.T, fn wakeRepairStarter) {
	t.Helper()
	old := startWakeFromTarget
	startWakeFromTarget = fn
	t.Cleanup(func() {
		startWakeFromTarget = old
	})
}

func TestWakeTargetWriteReadRoundTripAndPermissions(t *testing.T) {
	root := t.TempDir()
	injector := writeExecutableForTest(t, "injector")
	target := newWakeTarget(root, "codex", injector, []string{"exec", "target"})

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
	err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected world-writable rejection, got %v", err)
	}
}

func TestWakeTargetRejectsGroupWritableInjectVia(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o775); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected group-writable rejection, got %v", err)
	}
}

func TestWakeTargetRejectsNonOwnedInjectVia(t *testing.T) {
	oldCurrent := wakeTargetCurrentUID
	oldOwner := wakeTargetFileOwnerUID
	wakeTargetCurrentUID = func() (int, bool) { return 1000, true }
	wakeTargetFileOwnerUID = func(info os.FileInfo) (int, bool) { return 2000, true }
	t.Cleanup(func() {
		wakeTargetCurrentUID = oldCurrent
		wakeTargetFileOwnerUID = oldOwner
	})

	injector := writeExecutableForTest(t, "injector")
	err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "owned by uid 2000") {
		t.Fatalf("expected owner rejection, got %v", err)
	}
}

func TestReadWakeTargetRejectsUnsafeFileMode(t *testing.T) {
	root := t.TempDir()
	injector := writeExecutableForTest(t, "injector")
	target := newWakeTarget(root, "codex", injector, nil)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	if err := os.Chmod(wakeTargetPath(root, "codex"), 0o644); err != nil {
		t.Fatalf("chmod wake target: %v", err)
	}

	_, exists, err := readWakeTarget(root, "codex")
	if !exists {
		t.Fatal("expected unsafe target to be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "mode is 644, want 0600") {
		t.Fatalf("expected unsafe mode rejection, got %v", err)
	}
}

func TestReadWakeTargetRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "codex", newWakeTarget(root, "codex", injector, nil)); err != nil {
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

func TestRepairWakeRefusesRawTTYWithoutInjectTarget(t *testing.T) {
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run without target")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected repair refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "no inject-via wake target") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain on refused raw repair: %v", statErr)
	}
}

func TestRepairWakeRefusesUnverifiedLock(t *testing.T) {
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == 4242 {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	if err := writeWakeTarget(root, "codex", newWakeTarget(root, "codex", writeExecutableForTest(t, "injector"), nil)); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for unverified lock")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected unverified repair refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "unverified") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("unverified lock should remain: %v", statErr)
	}
}

func TestRepairWakeRemovesProvenStaleAndStartsFromTarget(t *testing.T) {
	root := t.TempDir()
	injector := writeExecutableForTest(t, "injector")
	target := newWakeTarget(root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, target wakeTarget) (int, error) {
		if gotRoot != root || gotMe != "codex" {
			t.Fatalf("start root/me = %q/%q", gotRoot, gotMe)
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("lock should be removed before start, stat=%v", err)
		}
		return 9876, nil
	})

	result, err := repairWake(root, "codex")
	if err != nil {
		t.Fatalf("repairWake: %v", err)
	}
	if result.Status != "repaired" || result.PID != 9876 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRepairWakeRefusesStaleRawLockWithLeftoverTarget(t *testing.T) {
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "codex", newWakeTarget(root, "codex", injector, []string{"exec"})); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for a target not bound to the lock")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected repair refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "not created for an inject-via repair target") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain on refused raw repair: %v", statErr)
	}
}

func TestRunWakeRepairCLIRefusesMissingLockWithJSON(t *testing.T) {
	root := t.TempDir()

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "codex", "--json"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "no wake lock present") {
		t.Fatalf("runWakeRepair error = %v, want missing-lock refusal", runErr)
	}

	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "refused" {
		t.Fatalf("status = %q, want refused; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Reason, "no wake lock present") {
		t.Fatalf("reason = %q, want missing-lock refusal", result.Reason)
	}
}

func TestRunDispatchWakeRepairCLIRefusesMissingLockWithJSON(t *testing.T) {
	root := t.TempDir()

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return Run([]string{"--no-update-check", "wake", "repair", "--root", root, "--me", "codex", "--json"}, "test")
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "no wake lock present") {
		t.Fatalf("Run error = %v, want missing-lock refusal", runErr)
	}

	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "no wake lock present") {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunWakeRepairCLIReportsAlreadyRunning(t *testing.T) {
	root := t.TempDir()
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		Executable:   "/opt/homebrew/bin/amq",
		ProcessStart: "start-token",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "start-token",
			Executable: "/opt/homebrew/bin/amq",
		}
	})
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for an already-running wake")
		return 0, nil
	})

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "codex"})
	})
	if runErr != nil {
		t.Fatalf("runWakeRepair: %v", runErr)
	}

	if !strings.Contains(stdout, "wake repair: already-running") || !strings.Contains(stdout, "pid=4242") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestRunWakeRepairCLIRepairsStaleWakeWithJSON(t *testing.T) {
	root := t.TempDir()
	injector := writeExecutableForTest(t, "injector")
	target := newWakeTarget(root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, target wakeTarget) (int, error) {
		if gotRoot != root || gotMe != "codex" {
			t.Fatalf("start root/me = %q/%q", gotRoot, gotMe)
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("lock should be removed before start, stat=%v", err)
		}
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

func TestBuildCoopWakeArgsIncludesInjectViaTarget(t *testing.T) {
	args := buildCoopWakeArgs("codex", "/tmp/root", "/abs/injector", []string{"exec", "target"})
	got := strings.Join(args, "|")
	want := "--no-update-check|wake|--me|codex|--root|/tmp/root|--inject-via|/abs/injector|--inject-arg|exec|--inject-arg|target"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
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

func TestBuildRepairWakeArgsIncludesReadyFileAndTarget(t *testing.T) {
	target := wakeTarget{
		InjectVia:  "/abs/injector",
		InjectArgs: []string{"exec", "target"},
	}
	args := buildRepairWakeArgs("/tmp/root", "codex", target, "/tmp/ready")
	got := strings.Join(args, "|")
	want := "--no-update-check|wake|--me|codex|--root|/tmp/root|--inject-via|/abs/injector|--inject-arg|exec|--inject-arg|target|--ready-file|/tmp/ready"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}
