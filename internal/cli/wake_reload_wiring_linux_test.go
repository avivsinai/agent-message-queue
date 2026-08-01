//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func linuxWakeReloadSocketPathsForTest(t *testing.T, root, agent string) []string {
	t.Helper()
	entries, err := os.ReadDir(fsq.AgentBase(root, agent))
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 1)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wr.") {
			paths = append(paths, filepath.Join(fsq.AgentBase(root, agent), entry.Name()))
		}
	}
	return paths
}

func TestRunWakeWithLoopStartsUnadvertisedLinuxReloadTransportForOrdinaryOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	ownerProcess := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := ownerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownerProcess.Process.Kill()
		_ = ownerProcess.Wait()
	})
	owner := authoritativeOwnerForPIDForCoopWakeTest(t, ownerProcess.Process.Pid)
	inspectProcess := inspectWakeProcess
	wakeProcess := inspectProcess(os.Getpid())
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			return inspectProcess(pid)
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: wakeProcess.StartToken,
			BootID:     wakeProcess.BootID,
			Executable: "/usr/local/bin/amq",
			Args: []string{
				"amq", "wake", "--root", root, "--me", "codex",
				"--inject-mode", wakeInjectModeNone, "--interrupt=false",
			},
		}
	})
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, encoded)

	var endpointPath string
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-mode", wakeInjectModeNone,
		"--interrupt=false",
	}, func(wakeConfig) error {
		paths := linuxWakeReloadSocketPathsForTest(t, root, "codex")
		if len(paths) != 1 {
			t.Fatalf("Linux reload endpoint paths = %#v, want exactly one", paths)
		}
		endpointPath = paths[0]
		info, err := os.Lstat(endpointPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("Linux reload endpoint mode = %v", info.Mode())
		}

		inspection := inspectWakeLock(root, "codex")
		if inspection.Lock.ResumeSchema != 0 || inspection.Lock.ResumeOwner != nil ||
			inspection.Lock.ControlSocket != "" || inspection.Lock.RunningImageEvidence != nil {
			t.Fatalf("Linux reload endpoint advertised resume metadata: %#v", inspection.Lock)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpointPath == "" {
		t.Fatal("Linux reload endpoint was not observed")
	}
	if _, err := os.Lstat(endpointPath); !os.IsNotExist(err) {
		t.Fatalf("Linux reload endpoint survived wake cleanup: %v", err)
	}
}

func TestRunWakeWithLoopClassifiesOptionalLinuxReloadTransportStartFailures(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	ownerProcess := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := ownerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownerProcess.Process.Kill()
		_ = ownerProcess.Wait()
	})
	owner := authoritativeOwnerForPIDForCoopWakeTest(t, ownerProcess.Process.Pid)
	inspectProcess := inspectWakeProcess
	wakeProcess := inspectProcess(os.Getpid())
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			return inspectProcess(pid)
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: wakeProcess.StartToken,
			BootID:     wakeProcess.BootID,
			Executable: "/usr/local/bin/amq",
			Args: []string{
				"amq", "wake", "--root", root, "--me", "codex",
				"--inject-mode", wakeInjectModeNone, "--interrupt=false",
			},
		}
	})
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, encoded)

	startCalled := false
	startFailure := errors.New("test reload bind failure")
	oldStart := startWakeReloadTransportForWake
	startWakeReloadTransportForWake = func(
		_ *wakeAgentDir,
		gotRoot string,
		gotAgent string,
		inspection wakeLockInspection,
		gotOwner wakeOwner,
	) (func(), error) {
		startCalled = true
		if gotRoot != canonicalWakeRoot(root) || gotAgent != "codex" || gotOwner != owner {
			t.Fatalf("reload start identity = root %q agent %q owner %#v", gotRoot, gotAgent, gotOwner)
		}
		if !inspection.IdentityConfirmed {
			t.Fatal("reload start did not receive confirmed lock identity")
		}
		return nil, &wakeReloadTransportUnavailableError{err: startFailure}
	}
	t.Cleanup(func() { startWakeReloadTransportForWake = oldStart })

	loopCalled := false
	var runErr error
	stderr := captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		}, func(wakeConfig) error {
			loopCalled = true
			if paths := linuxWakeReloadSocketPathsForTest(t, root, "codex"); len(paths) != 0 {
				t.Fatalf("failed Linux reload endpoint paths = %#v", paths)
			}
			return nil
		})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !startCalled || !loopCalled {
		t.Fatalf("reload start called = %v, loop called = %v", startCalled, loopCalled)
	}
	if !strings.Contains(stderr, "reload transport unavailable: "+startFailure.Error()) ||
		!strings.Contains(stderr, "continuing without reload transport") {
		t.Fatalf("reload transport degradation note = %q", stderr)
	}

	authorityFailure := errors.New("wake reload owner exited during startup")
	startCalled = false
	loopCalled = false
	startWakeReloadTransportForWake = func(
		_ *wakeAgentDir,
		_ string,
		_ string,
		_ wakeLockInspection,
		_ wakeOwner,
	) (func(), error) {
		startCalled = true
		return nil, authorityFailure
	}
	stderr = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		}, func(wakeConfig) error {
			loopCalled = true
			return nil
		})
	})
	if !errors.Is(runErr, authorityFailure) || !startCalled || loopCalled {
		t.Fatalf("authority failure result = %v, start called = %v, loop called = %v", runErr, startCalled, loopCalled)
	}
	if strings.Contains(stderr, "continuing without reload transport") {
		t.Fatalf("authority failure was narrated as degradable: %q", stderr)
	}

	startCalled = false
	loopCalled = false
	startWakeReloadTransportForWake = func(
		_ *wakeAgentDir,
		_ string,
		_ string,
		_ wakeLockInspection,
		_ wakeOwner,
	) (func(), error) {
		startCalled = true
		return nil, nil
	}
	runErr = runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-mode", wakeInjectModeNone,
		"--interrupt=false",
	}, func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "returned no cleanup") ||
		!startCalled || loopCalled {
		t.Fatalf("nil cleanup result = %v, start called = %v, loop called = %v", runErr, startCalled, loopCalled)
	}
}

func TestRunWakeWithLoopDoesNotStartLinuxReloadTransportOutsideOrdinaryOwner(t *testing.T) {
	t.Run("ownerless", func(t *testing.T) {
		root := secureTempDirForTest(t)
		ensureCoopWakeMailboxForTest(t, root, "codex")
		t.Setenv(envWakeOwner, "")

		err := runWakeWithLoop([]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		}, func(wakeConfig) error {
			if paths := linuxWakeReloadSocketPathsForTest(t, root, "codex"); len(paths) != 0 {
				t.Fatalf("ownerless Linux reload endpoint paths = %#v", paths)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("inject-via", func(t *testing.T) {
		root := secureTempDirForTest(t)
		ensureCoopWakeMailboxForTest(t, root, "codex")
		owner := currentAuthoritativeOwnerForCoopWakeTest(t)
		encoded, err := encodeWakeOwnerEnv(owner)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(envWakeOwner, encoded)
		injector := writeExecutableForTest(t, "linux-reload-injector")

		err = runWakeWithLoop([]string{
			"--root", root,
			"--me", "codex",
			"--inject-via", injector,
			"--interrupt=false",
		}, func(wakeConfig) error {
			if paths := linuxWakeReloadSocketPathsForTest(t, root, "codex"); len(paths) != 0 {
				t.Fatalf("inject-via Linux reload endpoint paths = %#v", paths)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unconfirmed lock identity", func(t *testing.T) {
		root := secureTempDirForTest(t)
		ensureCoopWakeMailboxForTest(t, root, "codex")
		owner := currentAuthoritativeOwnerForCoopWakeTest(t)
		encoded, err := encodeWakeOwnerEnv(owner)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(envWakeOwner, encoded)

		ownerDone := make(chan struct{})
		oldObserve := observeAuthoritativeWakeOwner
		observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
			if got != owner {
				t.Fatalf("observed owner = %#v, want %#v", got, owner)
			}
			return wakeOwnerObservation{State: wakeOwnerSame, done: ownerDone}, nil
		}
		t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

		process := inspectWakeProcess(os.Getpid())
		stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
			if pid != os.Getpid() {
				return inspectWakeProcessPlatform(pid)
			}
			process.Executable = "/tmp/not-amq"
			return process
		})
		startCalled := false
		oldStart := startWakeReloadTransportForWake
		startWakeReloadTransportForWake = func(
			_ *wakeAgentDir,
			_ string,
			_ string,
			_ wakeLockInspection,
			_ wakeOwner,
		) (func(), error) {
			startCalled = true
			return nil, errors.New("reload transport must not start for unconfirmed identity")
		}
		t.Cleanup(func() { startWakeReloadTransportForWake = oldStart })

		err = runWakeWithLoop([]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		}, func(wakeConfig) error {
			inspection := inspectWakeLock(root, "codex")
			if inspection.IdentityConfirmed {
				t.Fatal("test lock identity unexpectedly confirmed")
			}
			if paths := linuxWakeReloadSocketPathsForTest(t, root, "codex"); len(paths) != 0 {
				t.Fatalf("unconfirmed-identity Linux reload endpoint paths = %#v", paths)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if startCalled {
			t.Fatal("reload transport start was attempted for unconfirmed identity")
		}
	})
}
