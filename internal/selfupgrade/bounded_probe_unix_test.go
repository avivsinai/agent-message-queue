//go:build darwin || linux

package selfupgrade

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const boundedProbeTestPIDEnv = "AMQ_TEST_BOUNDED_PROBE_PID_FILE"

func TestRunBoundedProbeKillsDetachedStdioChildAfterSuccess(t *testing.T) {
	pid, output, err := runBoundedProbeShellTest(t, `
sleep 60 >/dev/null 2>&1 &
printf '%s' "$!" > "$AMQ_TEST_BOUNDED_PROBE_PID_FILE"
printf 'probe-ok\n'
`)
	if err != nil {
		t.Fatalf("RunBoundedProbe() error = %v, want success", err)
	}
	if string(output) != "probe-ok\n" {
		t.Fatalf("RunBoundedProbe() output = %q, want probe-ok", output)
	}
	waitForBoundedProbeChildExit(t, pid)
}

func TestRunBoundedProbeKillsChildHoldingStdoutAfterWaitDelay(t *testing.T) {
	started := time.Now()
	pid, _, err := runBoundedProbeShellTest(t, `
sleep 60 &
printf '%s' "$!" > "$AMQ_TEST_BOUNDED_PROBE_PID_FILE"
printf 'probe-ok\n'
`)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("RunBoundedProbe() error = %v, want ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > boundedProbeWaitDelay+2*time.Second {
		t.Fatalf("RunBoundedProbe() took %v, want at most %v", elapsed, boundedProbeWaitDelay+2*time.Second)
	}
	waitForBoundedProbeChildExit(t, pid)
}

func runBoundedProbeShellTest(t *testing.T, script string) (int, []byte, error) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	env := append(os.Environ(), boundedProbeTestPIDEnv+"="+pidPath)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := RunBoundedProbe(
		ctx,
		shell,
		[]string{"-c", script},
		BoundedProbeOptions{Env: env},
	)
	data, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("read child PID: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(string(data))
	if parseErr != nil || pid <= 0 {
		t.Fatalf("child PID = %q, parse error = %v", data, parseErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid, output, err
}

func waitForBoundedProbeChildExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child PID %d is still alive after bounded probe cleanup: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
