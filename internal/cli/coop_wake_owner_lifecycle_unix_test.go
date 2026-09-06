//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const ()

func TestCoopWakeAllModesInstallExactOwnerSupervisor(t *testing.T) {
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatalf("encode owner: %v", err)
	}

	oldAvailable := wakeTIOCSTIAvailable
	oldIsTTY := wakeInputIsTTY
	oldBindTerminalAuthority := bindWakeTerminalAuthorityForWake
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return true }
	bindWakeTerminalAuthorityForWake = func(
		generation wakeLockInspection,
		stop <-chan struct{},
	) (*wakeTerminalAuthority, error) {
		if !generation.Exists || generation.Lock.Generation == "" {
			return nil, errors.New("test terminal authority received no exact generation")
		}
		return &wakeTerminalAuthority{
			generation:  generation,
			controlStop: stop,
		}, nil
	}
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldIsTTY
		bindWakeTerminalAuthorityForWake = oldBindTerminalAuthority
	})

	for _, mode := range []string{
		wakeInjectModeAuto,
		wakeInjectModeRaw,
		wakeInjectModePaste,
		wakeInjectModeNone,
	} {
		t.Run(mode, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			t.Setenv(envWakeOwner, encoded)
			t.Setenv(envWakePrivateStopFD, "")

			loopCalled := false
			err := runWakeWithLoop(
				[]string{
					"--root", root,
					"--me", "codex",
					"--inject-mode", mode,
					"--interrupt=false",
				},
				func(cfg wakeConfig) error {
					loopCalled = true
					if cfg.controlStop == nil {
						return errors.New("coop-owned wake reached its loop without an exact owner supervisor")
					}
					select {
					case <-cfg.controlStop:
						return errors.New("live exact owner supervisor was already stopped")
					default:
					}
					return nil
				},
			)
			if err != nil {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if !loopCalled {
				t.Fatalf("mode %s: wake loop was not reached", mode)
			}
		})
	}
}

func TestCoopWakeOwnerMetadataFailureNeverStartsOwnerlessWake(t *testing.T) {
	tests := []struct {
		name       string
		ownerEnv   string
		observeErr error
	}{
		{
			name:     "malformed owner metadata",
			ownerEnv: `{"pid":`,
		},
		{
			name:     "unsupported exact owner observer",
			ownerEnv: mustEncodedCurrentOwnerForCoopWakeTest(t),
			observeErr: &wakeOwnerChildCapabilityUnsupportedError{
				Err: syscall.ENOSYS,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			t.Setenv(envWakeOwner, test.ownerEnv)
			t.Setenv(envWakePrivateStopFD, "")

			if test.observeErr != nil {
				oldObserve := observeAuthoritativeWakeOwner
				observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
					return wakeOwnerObservation{
						State:                 wakeOwnerUnknown,
						Reason:                "test observer unavailable",
						CapabilityUnsupported: true,
					}, test.observeErr
				}
				t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			}

			loopCalled := false
			childErr := runWakeWithLoop(
				[]string{
					"--root", root,
					"--me", "codex",
					"--inject-mode", wakeInjectModeNone,
					"--interrupt=false",
				},
				func(wakeConfig) error {
					loopCalled = true
					return errors.New("ownerless wake loop ran")
				},
			)
			if childErr == nil {
				t.Fatal("owner supervision failure started an ownerless wake")
			}
			if loopCalled {
				t.Fatal("owner supervision failure reached the wake loop")
			}
			if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
				t.Fatalf("owner supervision failure published a wake lock: %#v", inspection)
			}

			if err := handleCoopWakeSetupFailure(false, wakeLockInspection{}, "start coop-owned wake", childErr); err != nil {
				t.Fatalf("optional wake should continue with no wake, got %v", err)
			}
			if err := handleCoopWakeSetupFailure(true, wakeLockInspection{}, "start coop-owned wake", childErr); err == nil {
				t.Fatal("--require-wake accepted an unavailable exact owner observer")
			}
		})
	}
}

func TestKernelOwnerObserverIgnoresDescendantInheritedFDs(t *testing.T) {
	for _, exitKind := range []string{"normal", "crash"} {
		t.Run(exitKind, func(t *testing.T) {
			dir := t.TempDir()
			descendantPath := filepath.Join(dir, "descendant.pid")
			exitGate := filepath.Join(dir, "owner-exit")

			readEnd, writeEnd, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer func() { _ = readEnd.Close() }()

			script := `
sleep 30 &
printf '%s\n' "$!" > "$1"
while [ ! -e "$2" ]; do sleep 0.01; done
if [ "$3" = crash ]; then
	kill -KILL $$
fi
exit 0
`
			cmd := exec.Command("/bin/sh", "-c", script, "sh", descendantPath, exitGate, exitKind)
			cmd.ExtraFiles = []*os.File{writeEnd}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				_ = writeEnd.Close()
				t.Fatalf("start owner: %v", err)
			}
			ownerProcessGroup := cmd.Process.Pid
			t.Cleanup(func() {
				_ = syscall.Kill(-ownerProcessGroup, syscall.SIGKILL)
				if cmd.Process != nil && cmd.ProcessState == nil {
					_, _ = cmd.Process.Wait()
				}
			})
			_ = writeEnd.Close()

			waitForCoopWakePathForTest(t, descendantPath, 3*time.Second)
			owner := authoritativeOwnerForPIDForCoopWakeTest(t, cmd.Process.Pid)
			observation, err := observeAuthoritativeWakeOwner(owner)
			if err != nil {
				t.Fatalf("observe exact owner: %v", err)
			}
			defer func() { _ = observation.Close() }()
			if observation.State != wakeOwnerSame {
				t.Fatalf("owner observation = %#v, want live exact owner", observation)
			}
			if observation.Done() == nil {
				t.Fatal("kernel owner observation has no death signal")
			}

			if err := os.WriteFile(exitGate, []byte("exit\n"), 0o600); err != nil {
				t.Fatalf("release owner: %v", err)
			}
			waitErr := cmd.Wait()
			if exitKind == "normal" && waitErr != nil {
				t.Fatalf("normal owner exit: %v", waitErr)
			}
			if exitKind == "crash" && waitErr == nil {
				t.Fatal("crashing owner exited successfully")
			}

			descendantPID := readPIDForCoopWakeTest(t, descendantPath)
			descendant, err := os.FindProcess(descendantPID)
			if err != nil {
				t.Fatalf("find descendant: %v", err)
			}
			if err := descendant.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("FD-inheriting descendant is not alive: %v", err)
			}

			if err := readEnd.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatalf("set pipe deadline: %v", err)
			}
			var one [1]byte
			_, pipeErr := readEnd.Read(one[:])
			var netErr net.Error
			if !errors.As(pipeErr, &netErr) || !netErr.Timeout() {
				t.Fatalf("ordinary inherited pipe reported owner death while descendant lived: %v", pipeErr)
			}

			select {
			case <-observation.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("kernel-bound observation did not report exact owner exit")
			}
		})
	}
}

func TestOwnerDeathGatesTerminalInjection(t *testing.T) {
	oldInject := tiocstiInject
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		tiocstiInject = oldInject
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	for _, mode := range []string{wakeInjectModeRaw, wakeInjectModePaste} {
		t.Run(mode+"/already-dead", func(t *testing.T) {
			stop := make(chan struct{})
			close(stop)
			calls := 0
			tiocstiInject = func(string) error {
				calls++
				return nil
			}
			err := injectNotification(&wakeConfig{
				me:          "codex",
				injectMode:  mode,
				controlStop: stop,
			}, "must not inject", false)
			if err != nil {
				t.Fatalf("closed owner gate: %v", err)
			}
			if calls != 0 {
				t.Fatalf("closed owner gate allowed %d terminal injection(s)", calls)
			}
		})

		t.Run(mode+"/dies-during-injection", func(t *testing.T) {
			stop := make(chan struct{})
			var calls []string
			tiocstiInject = func(text string) error {
				calls = append(calls, text)
				if len(calls) == 1 {
					close(stop)
				}
				return nil
			}
			cfg := &wakeConfig{
				me:          "codex",
				injectMode:  mode,
				controlStop: stop,
			}
			err := injectNotification(cfg, "one in-flight write at most", false)
			var partial *wakeTerminalPartialProgressError
			if !errors.As(err, &partial) {
				t.Fatalf("mid-injection owner death error = %v, want retained partial progress", err)
			}
			if !cfg.inputDelivery.pending() {
				t.Fatal("mid-injection owner death discarded retained delivery progress")
			}
			if len(calls) != 1 {
				t.Fatalf("owner death allowed follow-up terminal injection: %#v", calls)
			}
		})
	}
}

func TestCoopRawDoorbellIsFixedASCIIAndShellInert(t *testing.T) {
	oldInject := tiocstiInject
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		tiocstiInject = oldInject
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	stop := make(chan struct{})
	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		return nil
	}
	dynamic := "AMQ [session/a]; sender=$(touch /tmp/pwned) subject='approve?' count=999 custom=\x1b[31m"
	err := injectNotification(&wakeConfig{
		me:          "codex",
		injectMode:  wakeInjectModeRaw,
		controlStop: stop,
	}, dynamic, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) == 0 || injected[0] != coopWakeDoorbell {
		t.Fatalf("coop text injection = %#v, want fixed first chunk %q", injected, coopWakeDoorbell)
	}
	for _, chunk := range injected {
		for _, value := range []byte(chunk) {
			if value > 0x7f {
				t.Fatalf("coop injection contains non-ASCII byte %#x: %#v", value, injected)
			}
		}
	}
	if strings.Contains(strings.Join(injected, ""), "session/a") ||
		strings.Contains(strings.Join(injected, ""), "sender=") ||
		strings.Contains(strings.Join(injected, ""), "approve") ||
		strings.Contains(strings.Join(injected, ""), "999") ||
		strings.Contains(strings.Join(injected, ""), "\x1b") {
		t.Fatalf("coop injection leaked message-derived bytes: %#v", injected)
	}
}

func TestExactWakeCleanupPreservesReplacementGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("acquire original wake: %v", err)
	}
	defer cleanup()
	original := inspectWakeLock(root, "codex")
	if !original.Exists || original.Lock.Generation == "" {
		t.Fatalf("original wake = %#v", original)
	}

	replacement := original.Lock
	replacement.Generation = "replacement-generation"
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatalf("encode replacement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock"), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("publish replacement: %v", err)
	}

	if err := cleanupTerminatedWakeLock(original); err != nil {
		t.Fatalf("old generation cleanup: %v", err)
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != replacement.Generation {
		t.Fatalf("old cleanup removed replacement generation: %#v", current)
	}
}

func TestStandaloneWakeRemainsOwnerless(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	t.Setenv(envWakeOwner, "")
	t.Setenv(envWakePrivateStopFD, "")

	loopCalled := false
	err := runWakeWithLoop(
		[]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		},
		func(cfg wakeConfig) error {
			loopCalled = true
			if cfg.controlStop != nil {
				return errors.New("standalone wake unexpectedly gained a coop owner supervisor")
			}
			inspection := inspectWakeLock(root, "codex")
			if inspection.Lock.Owner != nil || inspection.Lock.OwnerSchema != 0 {
				return fmt.Errorf("standalone wake became owner-bound: %#v", inspection.Lock)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !loopCalled {
		t.Fatal("standalone wake loop was not reached")
	}
}

func currentAuthoritativeOwnerForCoopWakeTest(t *testing.T) wakeOwner {
	t.Helper()
	return authoritativeOwnerForPIDForCoopWakeTest(t, os.Getpid())
}

func authoritativeOwnerForPIDForCoopWakeTest(t *testing.T, pid int) wakeOwner {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		proc := inspectWakeProcess(pid)
		sessionID, sessionErr := getWakeProcessSID(pid)
		owner := wakeOwner{
			PID:          pid,
			ProcessStart: proc.StartToken,
			BootID:       proc.BootID,
			SessionID:    sessionID,
		}
		if proc.Running && sessionErr == nil && validateAuthoritativeWakeOwner(owner) == nil {
			return owner
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture authoritative owner pid %d: proc=%#v sid=%d sidErr=%v", pid, proc, sessionID, sessionErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustEncodedCurrentOwnerForCoopWakeTest(t *testing.T) string {
	t.Helper()
	encoded, err := encodeWakeOwnerEnv(currentAuthoritativeOwnerForCoopWakeTest(t))
	if err != nil {
		t.Fatalf("encode current owner: %v", err)
	}
	return encoded
}

func ensureCoopWakeMailboxForTest(t *testing.T, root, me string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatalf("ensure mailbox: %v", err)
	}
}

func waitForCoopWakePathForTest(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

func readPIDForCoopWakeTest(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	return pid
}
