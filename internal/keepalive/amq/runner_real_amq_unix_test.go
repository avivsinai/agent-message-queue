//go:build darwin || linux

package amq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// These tests deliberately invoke a freshly built cmd/amq binary. The rest of
// this package proves StartWake's own readiness/cancellation/stderr-capture
// logic against fixture shell scripts; these tests instead prove that the
// real amq wake command accepts exactly the argv StartWake constructs and
// becomes ready, mirroring the real-binary E2E pattern in
// internal/cli/wake_p0_golden_abi_unix_test.go and wake_restart_e2e_unix_test.go.

const (
	realAMQBinaryBuildDirPrefix = "amq-keepalive-real-wake-bin-"
	realAMQBinaryStaleAge       = time.Hour
)

var realAMQBinaryBuildDir string

func TestMain(m *testing.M) {
	sweepStaleRealAMQBinaryBuildDirs(os.TempDir(), time.Now(), os.Stderr)
	code, err := withRealAMQBinaryBuildDir("", func(dir string) int {
		realAMQBinaryBuildDir = dir
		return m.Run()
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amq keepalive test binary cleanup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func sweepStaleRealAMQBinaryBuildDirs(tempRoot string, now time.Time, diagnostic io.Writer) {
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		_, _ = fmt.Fprintf(diagnostic, "inspect stale real amq binary build directories: %v\n", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), realAMQBinaryBuildDirPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			_, _ = fmt.Fprintf(diagnostic, "inspect stale real amq binary build directory %q: %v\n", entry.Name(), err)
			continue
		}
		if now.Sub(info.ModTime()) < realAMQBinaryStaleAge {
			continue
		}
		path := filepath.Join(tempRoot, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			_, _ = fmt.Fprintf(diagnostic, "remove stale real amq binary build directory %q: %v\n", path, err)
		}
	}
}

func withRealAMQBinaryBuildDir(tempRoot string, run func(string) int) (code int, retErr error) {
	dir, err := os.MkdirTemp(tempRoot, realAMQBinaryBuildDirPrefix)
	if err != nil {
		return 1, fmt.Errorf("create real amq binary build dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			code = 1
			retErr = fmt.Errorf("remove real amq binary build dir %q: %w", dir, err)
		}
	}()
	return run(dir), nil
}

var buildRealAMQBinaryOnce = sync.OnceValues(func() (string, error) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve real amq test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	if realAMQBinaryBuildDir == "" {
		return "", errors.New("real amq binary build directory is not initialized")
	}
	binary := filepath.Join(realAMQBinaryBuildDir, "amq")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/amq")
	cmd.Dir = repoRoot
	cmd.Env = realAMQCleanEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("build real amq binary timed out: %w\n%s", ctx.Err(), output)
	}
	if err != nil {
		return "", fmt.Errorf("build real amq binary: %w\n%s", err, output)
	}
	return binary, nil
})

func TestRealAMQBinaryBuildDirIsRemoved(t *testing.T) {
	tempRoot := t.TempDir()
	var buildDir string
	code, err := withRealAMQBinaryBuildDir(tempRoot, func(dir string) int {
		buildDir = dir
		if err := os.WriteFile(filepath.Join(dir, "amq"), []byte("fixture"), 0o700); err != nil {
			t.Fatalf("write fixture binary: %v", err)
		}
		return 23
	})
	if err != nil || code != 23 {
		t.Fatalf("withRealAMQBinaryBuildDir code=%d err=%v", code, err)
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Fatalf("build directory still exists after run: %v", err)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatalf("read temp root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp root contains leftovers: %#v", entries)
	}
}

func TestSweepStaleRealAMQBinaryBuildDirsPreservesFreshRun(t *testing.T) {
	tempRoot := t.TempDir()
	stale := filepath.Join(tempRoot, realAMQBinaryBuildDirPrefix+"stale")
	fresh := filepath.Join(tempRoot, realAMQBinaryBuildDirPrefix+"fresh")
	for _, dir := range []string{stale, fresh} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
	}
	now := time.Now()
	old := now.Add(-2 * realAMQBinaryStaleAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age stale directory: %v", err)
	}

	var diagnostic strings.Builder
	sweepStaleRealAMQBinaryBuildDirs(tempRoot, now, &diagnostic)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory still exists: %v", err)
	}
	if info, err := os.Stat(fresh); err != nil || !info.IsDir() {
		t.Fatalf("fresh directory was removed: info=%v err=%v", info, err)
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("unexpected sweep diagnostic: %s", diagnostic.String())
	}
}

func realAMQBinaryForTest(t *testing.T) string {
	t.Helper()
	binary, err := buildRealAMQBinaryOnce()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return binary
}

// realAMQCleanEnv strips ambient AMQ session/root identity so a real amq
// child process resolves only the --root this test passed explicitly,
// mirroring internal/cli/wake_p0_golden_abi_unix_test.go's wakeABICleanEnv.
func realAMQCleanEnv() []string {
	env := os.Environ()
	for _, name := range []string{
		"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION",
		"AMQ_GLOBAL_ROOT", "AMQ_WAKE_OWNER", "AMQ_WAKE_PRIVATE_STOP_FD",
	} {
		env = environmentWithout(env, name)
	}
	return append(env, "AMQ_NO_UPDATE_CHECK=1", "AMQ_WAKE_NO_SELF_UPGRADE=1")
}

func clearAMQSessionIdentityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION",
		"AMQ_GLOBAL_ROOT", "AMQ_WAKE_OWNER", "AMQ_WAKE_PRIVATE_STOP_FD",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("AMQ_NO_UPDATE_CHECK", "1")
	t.Setenv("AMQ_WAKE_NO_SELF_UPGRADE", "1")
}

// secureAMQTestRoot returns a fresh directory rooted under $HOME rather than
// the system temp directory. amq's --inject-via validation walks every
// ancestor directory and refuses a group/world-writable one; /tmp itself is
// typically mode 1777 and would fail that walk. This mirrors
// internal/cli/main_test.go's cliSecureTempRoot / secureTempDirForTest.
func secureAMQTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home directory: %v", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve home directory symlinks: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".amq-keepalive-real-wake-test-")
	if err != nil {
		t.Fatalf("create secure test root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove secure test root: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure test root: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve secure test root symlinks: %v", err)
	}
	return resolved
}

func writeRealAMQInjector(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "inject.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write inject-via executable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve inject-via executable: %v", err)
	}
	return resolved
}

// indexOfSubsequence returns the index at which needle first occurs as a
// contiguous run inside haystack, or -1.
func indexOfSubsequence(haystack, needle []string) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, want := range needle {
			if haystack[i+j] != want {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func realWakeReadinessDirEmpty(t *testing.T, cacheDir string) {
	t.Helper()
	readinessDir := filepath.Join(cacheDir, "amq-keepalive", "readiness")
	entries, err := os.ReadDir(readinessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read readiness dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "wake-") {
			t.Fatalf("ready marker %q leaked in %s", entry.Name(), readinessDir)
		}
	}
}

// TestStartWakeRealAMQBinaryBecomesReadyAndMatchesTarget starts a real amq
// wake process (not a fixture) through StartWake against a freshly
// initialized queue, and proves the running process itself — inspected via
// its own argv and via `amq wake check` — was launched with exactly the
// target/args StartWake constructed.
func TestStartWakeRealAMQBinaryBecomesReadyAndMatchesTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("real amq wake E2E")
	}
	binary := realAMQBinaryForTest(t)
	root := secureAMQTestRoot(t)
	const handle = "codex"

	initCmd := exec.Command(binary, "init", "--root", root, "--agents", handle)
	initCmd.Env = realAMQCleanEnv()
	if output, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("amq init: %v\n%s", err, output)
	}

	injector := writeRealAMQInjector(t, root)
	cacheDir := filepath.Join(root, "cache")
	clearAMQSessionIdentityEnv(t)
	t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", cacheDir)

	const adapter = "real-amq-e2e"
	const target = "real-amq-e2e:surface:startwake-test"
	req := StartWakeRequest{
		Root:      root,
		Me:        handle,
		InjectVia: injector,
		Adapter:   adapter,
		Target:    target,
		Timeout:   20 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := NewCLI(binary).StartWake(ctx, req); err != nil {
		t.Fatalf("StartWake() against real amq binary: %v", err)
	}

	lockPath := filepath.Join(fsq.AgentBase(root, handle), ".wake.lock")
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read real wake lock: %v", err)
	}
	var lock struct {
		PID      int      `json:"pid"`
		WakeMode string   `json:"wake_mode"`
		Args     []string `json:"args"`
	}
	if err := json.Unmarshal(lockData, &lock); err != nil {
		t.Fatalf("parse real wake lock: %v", err)
	}
	if lock.PID <= 0 {
		t.Fatalf("wake lock pid = %d, want > 0", lock.PID)
	}
	if lock.WakeMode != "inject-via" {
		t.Fatalf("wake lock mode = %q, want inject-via", lock.WakeMode)
	}

	// Safety net only: the assertions below retire the real wake explicitly.
	// This just guarantees no detached daemon survives if an earlier
	// assertion aborts the test before that point.
	t.Cleanup(func() {
		if running, runErr := fakeWakeProcessRunning(lock.PID); runErr != nil {
			t.Errorf("inspect real wake pid %d during cleanup: %v", lock.PID, runErr)
		} else if running {
			forceStopDetachedWakeForTest(t, lock.PID)
		}
	})

	// wakeProcessInfo.Args (and therefore lock.Args) is only populated on
	// Linux — internal/cli/wake_process_darwin.go never reads argv for the
	// process it inspects. Where it is available, prove the real process's
	// own argv is exactly what StartWake constructed.
	if runtime.GOOS == "linux" {
		wantArgs := []string{
			"wake",
			"-root", root,
			"-me", handle,
			"-inject-via", injector,
			"-inject-arg", "inject",
			"-inject-arg", adapter,
			"-inject-arg", target,
			"--retry-until", "injected",
			"--accept-existing-wake",
			"-ready-file",
		}
		idx := indexOfSubsequence(lock.Args, wantArgs)
		if idx < 0 {
			t.Fatalf("real wake process argv = %#v, want contiguous %#v", lock.Args, wantArgs)
		}
		readyValueIdx := idx + len(wantArgs)
		if readyValueIdx >= len(lock.Args) || !filepath.IsAbs(lock.Args[readyValueIdx]) {
			t.Fatalf("real wake process -ready-file value missing or not absolute: argv = %#v", lock.Args)
		}
	}

	check, err := NewCLI(binary).checkWake(context.Background(), req)
	if err != nil {
		t.Fatalf("checkWake() against real amq binary: %v", err)
	}
	if !check.LiveWake {
		t.Fatal("checkWake() live_wake = false, want true after StartWake")
	}
	if check.ImageStatus != wakeImageCurrent {
		t.Fatalf("checkWake() image_status = %q, want %q", check.ImageStatus, wakeImageCurrent)
	}

	realWakeReadinessDirEmpty(t, cacheDir)

	// Cross-platform proof that the real amq wake stored exactly the
	// target/args StartWake passed: amq wake retire recomputes an identity
	// from (root, me, inject-via, inject-arg...) and refuses unless it
	// matches the persisted target byte-for-byte. A mismatched target must
	// be refused, with the specific stable reason amq's own target-identity
	// comparison (sameWakeInjectorIdentity in internal/cli/wake_target.go)
	// reports — not just any non-retired outcome, which a wrong generic
	// refusal (owner-bound, unverified identity, artifact error) would also
	// satisfy — and it must leave the live wake untouched.
	const wantMismatchReason = "saved wake target uses a different injector identity or retry acknowledgement policy"
	mismatched, mismatchErr := NewCLI(binary).RetireWake(context.Background(), RetireWakeRequest{
		Root: root, Me: handle, InjectVia: injector, Adapter: adapter, Target: target + "-wrong",
	})
	if mismatched.Retired() {
		t.Fatalf("RetireWake() with a mismatched target retired the real wake: %+v", mismatched)
	}
	if !errors.Is(mismatchErr, ErrWakeRetireNotConfirmed) {
		t.Fatalf("RetireWake() with a mismatched target error = %v, want ErrWakeRetireNotConfirmed", mismatchErr)
	}
	if mismatched.Status != "refused" || mismatched.Reason != wantMismatchReason {
		t.Fatalf("RetireWake() with a mismatched target = %+v, want status=refused reason=%q",
			mismatched, wantMismatchReason)
	}
	if stillLive, checkErr := NewCLI(binary).checkWake(context.Background(), req); checkErr != nil || !stillLive.LiveWake {
		t.Fatalf("wake did not survive a refused mismatched retire: live=%+v err=%v", stillLive, checkErr)
	}

	// ...while the exact identity StartWake used must retire it, and the
	// retirement must be real: the running process actually exits, the lock
	// file is gone, and a fresh amq wake check no longer sees it live. The
	// returned JSON claiming "retired" is not sufficient proof on its own.
	result, retireErr := NewCLI(binary).RetireWake(context.Background(), RetireWakeRequest{
		Root: root, Me: handle, InjectVia: injector, Adapter: adapter, Target: target,
	})
	if retireErr != nil || !result.Retired() {
		t.Fatalf("RetireWake() with the exact StartWake identity = %+v, err = %v, want retired", result, retireErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		running, runErr := fakeWakeProcessRunning(lock.PID)
		if runErr != nil {
			t.Fatalf("inspect retired wake pid %d: %v", lock.PID, runErr)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real wake process pid %d still alive after RetireWake() reported retired", lock.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("wake lock file %q still exists after retirement: %v", lockPath, statErr)
	}
	afterCheck, afterErr := NewCLI(binary).checkWake(context.Background(), req)
	if afterErr != nil {
		t.Fatalf("checkWake() after retirement: %v", afterErr)
	}
	if afterCheck.LiveWake {
		t.Fatalf("checkWake() live_wake = true after retirement, want false")
	}
}

// TestStartWakeRealAMQBinaryFailsForInvalidHandle proves StartWake surfaces a
// real, documented amq failure — rather than hanging or silently starting a
// wake — when asked to start against an invalid agent handle, and that no
// readiness marker is left behind.
func TestStartWakeRealAMQBinaryFailsForInvalidHandle(t *testing.T) {
	if testing.Short() {
		t.Skip("real amq wake E2E")
	}
	binary := realAMQBinaryForTest(t)
	root := secureAMQTestRoot(t)

	injector := writeRealAMQInjector(t, root)
	cacheDir := filepath.Join(root, "cache")
	clearAMQSessionIdentityEnv(t)
	t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", cacheDir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := NewCLI(binary).StartWake(ctx, StartWakeRequest{
		Root:      root,
		Me:        "Invalid-Handle", // fsq.ValidateHandle requires [a-z0-9_-]+
		InjectVia: injector,
		Adapter:   "real-amq-e2e",
		Target:    "real-amq-e2e:surface:invalid-handle",
		Timeout:   10 * time.Second,
	})
	if err == nil {
		t.Fatal("StartWake() error = nil, want failure for an invalid agent handle")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Fatalf("StartWake() error = %v, want the real amq handle-validation message", err)
	}

	realWakeReadinessDirEmpty(t, cacheDir)

	// No wake lock should exist for an identity that was never accepted.
	if _, statErr := os.Stat(filepath.Join(root, "agents", "Invalid-Handle", ".wake.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("wake lock exists after a refused invalid handle: %v", statErr)
	}
}
