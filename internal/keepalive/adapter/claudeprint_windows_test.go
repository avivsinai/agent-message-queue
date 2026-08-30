//go:build windows

package adapter

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const windowsProcessHelperEnv = "AMQ_TEST_WINDOWS_PROCESS_HELPER"
const windowsProcessGrandchildPIDEnv = "AMQ_TEST_WINDOWS_GRANDCHILD_PID"

func TestWindowsProcessHelper(t *testing.T) {
	switch os.Getenv(windowsProcessHelperEnv) {
	case "parent":
		_, _ = io.Copy(io.Discard, os.Stdin)
		executable, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(executable, "-test.run=^TestWindowsProcessHelper$")
		child.Env = replaceTestEnv(os.Environ(), windowsProcessHelperEnv, "grandchild")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv(windowsProcessGrandchildPIDEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(4)
		}
		_ = child.Wait()
		os.Exit(0)
	case "grandchild":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		return
	}
}

func TestWindowsClaudePrintSpawnerTerminatesItsProcessTree(t *testing.T) {
	t.Setenv(windowsProcessHelperEnv, "parent")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "child.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	grandchildPIDPath := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv(windowsProcessGrandchildPIDEnv, grandchildPIDPath)
	proc, err := (platformProcessSpawner{}).Start(context.Background(), processSpec{
		Path:    executable,
		Args:    []string{"-test.run=^TestWindowsProcessHelper$"},
		Dir:     t.TempDir(),
		Stdin:   []byte("payload\n"),
		LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = proc.KillGroup() })
	if alive, err := pidAlive(proc.PID()); err != nil || !alive {
		t.Fatalf("spawned pid alive = %v, %v; want true", alive, err)
	}
	grandchildPID := waitForTestPID(t, grandchildPIDPath)
	if alive, err := pidAlive(grandchildPID); err != nil || !alive {
		t.Fatalf("grandchild pid alive = %v, %v; want true", alive, err)
	}
	if err := proc.KillGroup(); err != nil {
		t.Fatalf("KillGroup() error = %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait() }()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not terminate within 5s")
	}
	if alive, err := pidAlive(proc.PID()); err != nil || alive {
		t.Fatalf("terminated pid alive = %v, %v; want false", alive, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive, aliveErr := pidAlive(grandchildPID)
		if aliveErr != nil {
			t.Fatalf("grandchild pid liveness: %v", aliveErr)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job termination left the grandchild process alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWindowsClaudePrintKillGroupFallsBackWhenJobHandleFails(t *testing.T) {
	t.Setenv(windowsProcessHelperEnv, "grandchild")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestWindowsProcessHelper$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := &windowsStartedProcess{cmd: cmd, job: windows.InvalidHandle}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := proc.KillGroup(); err != nil {
		t.Fatalf("KillGroup() fallback error = %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("fallback-killed process exited successfully; want a killed exit")
	}
}

func TestWindowsClaudePrintPIDLivenessDistinguishesLiveAndAbsent(t *testing.T) {
	if alive, err := pidAlive(os.Getpid()); err != nil || !alive {
		t.Fatalf("current pid alive = %v, %v; want true", alive, err)
	}
	if alive, err := pidAlive(2147483647); err != nil || alive {
		t.Fatalf("absent pid alive = %v, %v; want false", alive, err)
	}
}

func TestWindowsClaudePrintInjectLockIsExclusiveAndReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inject.lock")
	unlock, err := tryFlockExclusive(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := tryFlockExclusive(path); !errors.Is(err, errInjectBusy) {
		t.Fatalf("second lock error = %v, want errInjectBusy", err)
	}
	unlock()
	reunlock, err := tryFlockExclusive(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	reunlock()
}

func replaceTestEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(strings.ToUpper(item), prefix) {
			out = append(out, item)
		}
	}
	return append(out, key+"="+value)
}

func waitForTestPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("grandchild pid file = %q, %v", raw, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for grandchild pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
