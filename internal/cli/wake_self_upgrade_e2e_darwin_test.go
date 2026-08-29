//go:build darwin

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinWakeSelfUpgradeRealPTYStableSymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("real Darwin PTY self-upgrade E2E")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stable, oldBinary, newBinary, root, oldVersion, newVersion := prepareWakeSelfUpgradeE2E(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ptyLog := filepath.Join(t.TempDir(), "pty.log")
	// Register the spawned-wake reap BEFORE the test runs so that, under LIFO
	// cleanup ordering, it executes after the assertions but BEFORE the
	// TempDir RemoveAll cleanups. Without this the real-PTY wake daemon (a
	// detached child of `script`/`coop exec`) keeps writing to
	// root/agents/codex after the test returns and TempDir cleanup fails with
	// "directory not empty" (bead agent-message-queue-sqx). The PID is captured
	// from the helper's AMQ_SELF_UPGRADE_HELPER_OK line after CombinedOutput.
	var spawnedWakePID int
	t.Cleanup(func() {
		reapSpawnedWakeSelfUpgradePID(t, spawnedWakePID)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script", "-q", ptyLog,
		stable,
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", "raw",
		testBinary,
		"-test.run=^TestWakeSelfUpgradeRealPTYOwnerHelper$",
	)
	cmd.Env = wakeSelfUpgradeE2EEnv(root, stable, oldBinary, newBinary, oldVersion, newVersion)
	ptyInput, keepPTYInputOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptyInput.Close() }()
	defer func() { _ = keepPTYInputOpen.Close() }()
	cmd.Stdin = ptyInput
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Darwin self-upgrade PTY E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Darwin self-upgrade PTY E2E: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "AMQ_SELF_UPGRADE_HELPER_OK") {
		t.Fatalf("Darwin self-upgrade helper proof missing:\n%s", output)
	}
	spawnedWakePID = parseSpawnedWakePIDFromHelperOutput(t, string(output))
}

// parseSpawnedWakePIDFromHelperOutput extracts the wake daemon PID the helper
// prints as "AMQ_SELF_UPGRADE_HELPER_OK pid=<n> ...". Returns 0 if absent
// (reapSpawnedWakeSelfUpgradePID treats 0 as nothing-to-reap).
func parseSpawnedWakePIDFromHelperOutput(t *testing.T, output string) int {
	t.Helper()
	marker := "AMQ_SELF_UPGRADE_HELPER_OK pid="
	idx := strings.Index(output, marker)
	if idx < 0 {
		return 0
	}
	rest := output[idx+len(marker):]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		end = len(rest)
	}
	pidStr := strings.TrimSpace(rest[:end])
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		t.Fatalf("could not parse spawned wake pid from helper output: %q", output)
	}
	return pid
}

// reapSpawnedWakeSelfUpgradePID waits for the real-PTY wake daemon to exit
// after the test's assertions, using reap observation (not a fixed sleep) so
// TempDir cleanup runs against a quiescent root. If the PID is 0 (helper never
// printed proof) there is nothing to reap. If the child fails to exit within
// the bound the test fails explicitly, naming the pid — instead of surfacing as
// a TempDir "directory not empty" cleanup error.
func reapSpawnedWakeSelfUpgradePID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	// The wake daemon is detached; it will not exit on its own in the E2E
	// window. Signal it to stop, then observe actual exit via processAlive.
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Errorf("spawned wake pid %d: os.FindProcess: %v", pid, err)
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// SIGTERM did not settle it within the bound; escalate.
	_ = proc.Signal(syscall.SIGKILL)
	killDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(killDeadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("spawned wake pid %d did not exit within reap bound", pid)
}
