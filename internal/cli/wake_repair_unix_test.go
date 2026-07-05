//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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

func TestWriteWakeTargetRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "codex")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	targetPath := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(targetPath, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(targetPath, wakeTargetPath(root, "codex")); err != nil {
		t.Fatalf("symlink wake target: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	err := writeWakeTarget(root, "codex", target)
	if err == nil {
		t.Fatal("expected wake target symlink rejection")
	}
	if got, readErr := os.ReadFile(targetPath); readErr != nil || string(got) != "old\n" {
		t.Fatalf("symlink target changed: data=%q err=%v", got, readErr)
	}
}

func TestWriteWakeTargetSurvivesConcurrentCreate(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	start := make(chan struct{})
	errs := make(chan error, 8)

	for i := 0; i < cap(errs); i++ {
		i := i
		go func() {
			<-start
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{fmt.Sprintf("exec-%d", i)})
			errs <- writeWakeTarget(root, "codex", target)
		}()
	}
	close(start)
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatalf("writeWakeTarget concurrent writer %d: %v", i, err)
		}
	}

	if _, exists, err := readWakeTarget(root, "codex"); err != nil || !exists {
		t.Fatalf("readWakeTarget after concurrent writes: exists=%v err=%v", exists, err)
	}
	tmpMatches, err := filepath.Glob(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.target.tmp.*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("temporary wake target files remain: %v", tmpMatches)
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

func TestWakeTargetRejectsWorldWritableInjectViaAncestor(t *testing.T) {
	base := secureTempDirForTest(t)
	writableParent := filepath.Join(base, "writable")
	safeChild := filepath.Join(writableParent, "safe")
	if err := os.MkdirAll(safeChild, 0o700); err != nil {
		t.Fatalf("mkdir safe child: %v", err)
	}
	if err := os.Chmod(writableParent, 0o777); err != nil {
		t.Fatalf("chmod writable parent: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(writableParent, 0o700)
	})
	injector := filepath.Join(safeChild, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}

	_, err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected writable ancestor rejection, got %v", err)
	}
}

func TestWakeTargetResolvesLeafSymlinkInjectVia(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	link := filepath.Join(t.TempDir(), "injector-link")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	target := mustNewWakeTargetForTest(t, root, "codex", link, nil)
	if target.InjectVia != injector {
		t.Fatalf("target inject_via = %q, want resolved %q", target.InjectVia, injector)
	}
	if err := validateWakeTarget(target, root, "codex"); err != nil {
		t.Fatalf("validateWakeTarget: %v", err)
	}
}

func TestWakeTargetAllowsSymlinkedParentWhenResolvedPathIsSafe(t *testing.T) {
	base := secureTempDirForTest(t)
	realRoot := filepath.Join(base, "real-root")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatalf("mkdir real root: %v", err)
	}
	linkRoot := filepath.Join(base, "link-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatalf("symlink root: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, linkRoot, "codex", injector, nil)

	if err := writeWakeTarget(linkRoot, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget through symlinked root: %v", err)
	}
	if _, exists, err := readWakeTarget(linkRoot, "codex"); err != nil || !exists {
		t.Fatalf("readWakeTarget through symlinked root exists=%v err=%v", exists, err)
	}
}

func TestWakeTargetResolvesSymlinkedInjectViaParent(t *testing.T) {
	base := secureTempDirForTest(t)
	realDir := filepath.Join(base, "real-bin")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real bin: %v", err)
	}
	injector := filepath.Join(realDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	linkDir := filepath.Join(base, "link-bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink bin: %v", err)
	}

	target := mustNewWakeTargetForTest(t, base, "codex", filepath.Join(linkDir, "injector"), nil)
	if target.InjectVia != injector {
		t.Fatalf("target inject_via = %q, want resolved %q", target.InjectVia, injector)
	}
	if err := validateWakeTarget(target, base, "codex"); err != nil {
		t.Fatalf("validateWakeTarget: %v", err)
	}
}

func TestWakeTargetResolvesLeafSymlinkInjectViaForValidation(t *testing.T) {
	base := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	link := filepath.Join(base, "injector-link")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	target := mustNewWakeTargetForTest(t, base, "codex", link, nil)
	if target.InjectVia != injector {
		t.Fatalf("target inject_via = %q, want resolved %q", target.InjectVia, injector)
	}
	if err := validateWakeTarget(target, base, "codex"); err != nil {
		t.Fatalf("validateWakeTarget: %v", err)
	}
}

func TestWakeTargetRejectsDanglingSymlinkInjectVia(t *testing.T) {
	root := secureTempDirForTest(t)
	link := filepath.Join(root, "injector-link")
	if err := os.Symlink(filepath.Join(root, "missing-injector"), link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	_, err := newWakeTarget(root, "codex", link, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve inject_via") {
		t.Fatalf("expected dangling symlink rejection, got %v", err)
	}
}

func TestWakeTargetRejectsSymlinkToNonExecutableInjectVia(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := filepath.Join(root, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	link := filepath.Join(root, "injector-link")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	_, err := newWakeTarget(root, "codex", link, nil)
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected non-executable resolved target rejection, got %v", err)
	}
}

func TestWakeTargetRejectsSymlinkToNonOwnedInjectVia(t *testing.T) {
	oldCurrent := wakeTargetCurrentUID
	oldOwner := wakeTargetFileOwnerUID
	wakeTargetCurrentUID = func() (int, bool) { return 1000, true }
	wakeTargetFileOwnerUID = func(info os.FileInfo) (int, bool) { return 2000, true }
	t.Cleanup(func() {
		wakeTargetCurrentUID = oldCurrent
		wakeTargetFileOwnerUID = oldOwner
	})

	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	link := filepath.Join(root, "injector-link")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	_, err := newWakeTarget(root, "codex", link, nil)
	if err == nil || !strings.Contains(err.Error(), "owned by uid 2000") {
		t.Fatalf("expected owner rejection, got %v", err)
	}
}

func TestWakeTargetCreationRejectsMissingInjectVia(t *testing.T) {
	root := secureTempDirForTest(t)
	missing := filepath.Join(root, "missing-injector")

	_, err := newWakeTarget(root, "codex", missing, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve inject_via") {
		t.Fatalf("expected inject_via resolve failure, got %v", err)
	}
}

func TestWakeTargetRejectsGroupWritableInjectVia(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o775); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	_, err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected group-writable rejection, got %v", err)
	}
}

func TestWakeTargetRejectsNonRegularInjectVia(t *testing.T) {
	path := t.TempDir()

	_, err := validateWakeInjectViaPath(path)
	if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("expected non-regular rejection, got %v", err)
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
	_, err := validateWakeInjectViaPath(injector)
	if err == nil || !strings.Contains(err.Error(), "owned by uid 2000") {
		t.Fatalf("expected owner rejection, got %v", err)
	}
}

func TestReadWakeTargetRejectsNonRegularFile(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := os.MkdirAll(wakeTargetPath(root, "codex"), 0o700); err != nil {
		t.Fatalf("mkdir wake target path: %v", err)
	}

	_, exists, err := readWakeTarget(root, "codex")
	if !exists {
		t.Fatal("expected non-regular target to be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("expected non-regular target rejection, got %v", err)
	}
}

func TestReadWakeTargetRejectsFIFOWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := filepath.Dir(wakeTargetPath(root, "codex"))
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	if err := syscall.Mkfifo(wakeTargetPath(root, "codex"), 0o600); err != nil {
		t.Fatalf("mkfifo wake target: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, exists, err := readWakeTarget(root, "codex")
		if !exists {
			done <- fmt.Errorf("expected FIFO target to be reported present")
			return
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
			t.Fatalf("expected FIFO rejection, got %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("readWakeTarget blocked on FIFO")
	}
}

func TestReadWakeTargetRejectsUnsafeFileMode(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
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

func TestReadWakeTargetRejectsNonOwnedFile(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "codex", mustNewWakeTargetForTest(t, root, "codex", injector, nil)); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	oldCurrent := wakeTargetCurrentUID
	oldOwner := wakeTargetFileOwnerUID
	wakeTargetCurrentUID = func() (int, bool) { return 1000, true }
	wakeTargetFileOwnerUID = func(info os.FileInfo) (int, bool) { return 2000, true }
	t.Cleanup(func() {
		wakeTargetCurrentUID = oldCurrent
		wakeTargetFileOwnerUID = oldOwner
	})

	_, exists, err := readWakeTarget(root, "codex")
	if !exists {
		t.Fatal("expected non-owned target to be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "owned by uid 2000") {
		t.Fatalf("expected target owner rejection, got %v", err)
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

func TestReadWakeTargetRejectsWorldWritableParent(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := os.MkdirAll(filepath.Join(root, "agents", "codex"), 0o700); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "agents"), 0o777); err != nil {
		t.Fatalf("chmod agents dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(root, "agents"), 0o700)
	})
	targetPath := wakeTargetPath(root, "codex")
	if err := os.WriteFile(targetPath, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatalf("write wake target: %v", err)
	}

	_, exists, err := readWakeTarget(root, "codex")
	if !exists {
		t.Fatal("expected unsafe target to be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected writable parent rejection, got %v", err)
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
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
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

func TestRepairWakeRefusesStructurallyTamperedStaleLock(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for structurally tampered lock")
		return 0, nil
	})

	for _, tc := range []struct {
		name       string
		lock       wakeLock
		wantReason string
	}{
		{
			name: "invalid pid",
			lock: bindWakeLockToTarget(wakeLock{
				PID:   -1,
				Root:  canonicalWakeRoot(root),
				Agent: "codex",
			}, target),
			wantReason: "invalid pid",
		},
		{
			name: "missing root",
			lock: bindWakeLockToTarget(wakeLock{
				PID:   4242,
				Agent: "codex",
			}, target),
			wantReason: "lock root missing",
		},
		{
			name: "root mismatch",
			lock: bindWakeLockToTarget(wakeLock{
				PID:   4242,
				Root:  filepath.Join(root, "other-root"),
				Agent: "codex",
			}, target),
			wantReason: "root mismatch",
		},
		{
			name: "agent mismatch",
			lock: bindWakeLockToTarget(wakeLock{
				PID:   4242,
				Root:  canonicalWakeRoot(root),
				Agent: "other",
			}, target),
			wantReason: "agent mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lockPath := writeWakeLockExactForTest(t, root, "codex", tc.lock)

			result, err := repairWake(root, "codex")
			if err == nil {
				t.Fatal("expected repair refusal")
			}
			if result.Status != "refused" || !strings.Contains(result.Reason, tc.wantReason) ||
				!strings.Contains(result.Reason, "not repairable") {
				t.Fatalf("unexpected result: %#v err=%v", result, err)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("tampered lock should remain: %v", statErr)
			}
		})
	}
}

func TestRepairWakeRefusesRawTTYWithoutInjectTarget(t *testing.T) {
	root := secureTempDirForTest(t)
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
	root := secureTempDirForTest(t)
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
	if err := writeWakeTarget(root, "codex", mustNewWakeTargetForTest(t, root, "codex", writeExecutableForTest(t, "injector"), nil)); err != nil {
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

func TestRepairWakeRefusesCreatingLock(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := filepath.Dir(wakeTargetPath(root, "codex"))
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	lockPath := filepath.Join(agentBase, ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("write creating lock: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run for creating lock")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected creating-lock refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "being created") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("creating lock should remain: %v", statErr)
	}
}

func TestRepairWakeRefusesLockChangedBeforeRemoval(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		if calls == 1 {
			writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
				PID:        4243,
				Executable: "/opt/homebrew/bin/amq",
			}, target))
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
		t.Fatalf("startWakeFromTarget should not run after lock changed")
		return 0, nil
	})

	result, err := repairWake(root, "codex")
	if err == nil {
		t.Fatal("expected changed-lock refusal")
	}
	if result.Status != "refused" || !strings.Contains(result.Reason, "changed before repair") {
		t.Fatalf("unexpected result: %#v err=%v", result, err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("changed lock should remain: %v", statErr)
	}
}

func TestRepairWakeRemovesProvenStaleAndStartsFromTarget(t *testing.T) {
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
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "codex", mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})); err != nil {
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

func TestRepairWakeRefusesLiveIdentityMismatchLock(t *testing.T) {
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
		{
			name: "process start mismatch",
			lock: wakeLock{
				PID:          4242,
				ProcessStart: "recorded-start",
				BootID:       "same-boot",
				Executable:   "/opt/homebrew/bin/amq",
			},
			process: wakeProcessInfo{
				PID:        4242,
				Running:    true,
				StartToken: "actual-start",
				BootID:     "same-boot",
				Executable: "/opt/homebrew/bin/amq",
			},
			wantReason: "process start time mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(tc.lock, target))
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatalf("writeWakeTarget: %v", err)
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				proc := tc.process
				proc.PID = pid
				proc.Args = []string{"amq", "wake", "--root", root, "--me", "codex"}
				return proc
			})
			stubStartWakeFromTarget(t, func(root, me string, target wakeTarget) (int, error) {
				t.Fatalf("startWakeFromTarget should not run for live identity mismatch")
				return 0, nil
			})

			result, err := repairWake(root, "codex")
			if err == nil {
				t.Fatal("expected repair refusal")
			}
			if result.Status != "refused" ||
				!strings.Contains(result.Reason, "unverified") ||
				!strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("unexpected result: %#v err=%v", result, err)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("lock should remain on refused live identity mismatch: %v", statErr)
			}
		})
	}
}

func TestRunWakeRepairCLIRefusesMissingLockWithJSON(t *testing.T) {
	root := secureTempDirForTest(t)

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
	root := secureTempDirForTest(t)

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

func TestRunWakeRepairCLIRefusesValidLock(t *testing.T) {
	root := secureTempDirForTest(t)
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
	if runErr == nil || !strings.Contains(runErr.Error(), "already valid") {
		t.Fatalf("runWakeRepair error = %v, want valid-lock refusal", runErr)
	}

	if !strings.Contains(stdout, "wake repair: refused") ||
		!strings.Contains(stdout, "pid=4242") ||
		!strings.Contains(stdout, "already valid") {
		t.Fatalf("unexpected stdout: %q", stdout)
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

func TestRunWakeRepairClearsRepairAvailableAfterStartFailure(t *testing.T) {
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
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	stubStartWakeFromTarget(t, func(gotRoot, gotMe string, target wakeTarget) (int, error) {
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("lock should be removed before start, stat=%v", err)
		}
		return 0, errors.New("start failed")
	})

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "codex", "--json"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "start failed") {
		t.Fatalf("runWakeRepair error = %v, want start failure", runErr)
	}

	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "error" || result.RepairAvailable {
		t.Fatalf("repair_available should be cleared after start failure: %#v", result)
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
