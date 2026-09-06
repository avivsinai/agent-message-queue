//go:build darwin

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinWakeOwnerObservationSignalsExactOwnerExitWithLiveDescendant(t *testing.T) {
	for _, exitKind := range []string{"normal", "crash"} {
		t.Run(exitKind, func(t *testing.T) {
			cmd, owner, descendantPID, release := startDarwinWakeObservationOwner(t)
			observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
			if err != nil {
				t.Fatalf("observe exact owner: %v", err)
			}
			defer func() {
				if err := observation.Close(); err != nil {
					t.Errorf("close owner observation: %v", err)
				}
			}()
			if observation.State != wakeOwnerSame {
				t.Fatalf("owner observation = %#v, want live exact owner", observation)
			}
			if observation.Done() == nil {
				t.Fatal("live owner observation has no death signal")
			}

			if exitKind == "normal" {
				if err := os.WriteFile(release, []byte("exit\n"), 0o600); err != nil {
					t.Fatalf("release owner: %v", err)
				}
			} else if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("crash owner: %v", err)
			}
			waitErr := cmd.Wait()
			cmd.Process = nil
			if exitKind == "normal" && waitErr != nil {
				t.Fatalf("wait for normal owner exit: %v", waitErr)
			}
			if exitKind == "crash" && waitErr == nil {
				t.Fatal("crashed owner exited successfully")
			}

			descendant, err := os.FindProcess(descendantPID)
			if err != nil {
				t.Fatalf("find descendant: %v", err)
			}
			if err := descendant.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("descendant did not outlive exact owner: %v", err)
			}
			select {
			case <-observation.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("exact owner exit was prolonged by a live descendant")
			}
		})
	}
}

func startDarwinWakeObservationOwner(
	t *testing.T,
) (*exec.Cmd, wakeOwner, int, string) {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	descendantPath := filepath.Join(dir, "descendant.pid")
	script := `
sleep 30 &
printf '%s\n' "$!" > "$1"
: > "$2"
while [ ! -e "$3" ]; do sleep 0.01; done
exit 0
`
	cmd := exec.Command("/bin/sh", "-c", script, "sh", descendantPath, ready, release)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	ownerProcessGroup := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-ownerProcessGroup, syscall.SIGKILL)
		if cmd.Process != nil {
			_, _ = cmd.Process.Wait()
		}
	})
	waitForDarwinWakeObservationPath(t, ready)
	owner := authoritativeOwnerForDarwinWakeObservationTest(t, cmd.Process.Pid)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || descendantPID <= 0 {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	return cmd, owner, descendantPID, release
}

func authoritativeOwnerForDarwinWakeObservationTest(t *testing.T, pid int) wakeOwner {
	t.Helper()
	process := inspectWakeProcess(pid)
	sessionID, sessionErr := getWakeProcessSID(pid)
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: process.StartToken,
		BootID:       process.BootID,
		SessionID:    sessionID,
	}
	if !process.Running || sessionErr != nil {
		t.Fatalf("capture owner pid %d: process=%#v sessionErr=%v", pid, process, sessionErr)
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		t.Fatalf("validate owner pid %d: %v", pid, err)
	}
	return owner
}

func waitForDarwinWakeObservationPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
