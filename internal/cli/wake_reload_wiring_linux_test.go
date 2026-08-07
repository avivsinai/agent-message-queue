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

func TestRunWakeWithLoopAdvertisesSignalWithoutLinuxReloadTransport(t *testing.T) {
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

	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-mode", wakeInjectModeNone,
		"--interrupt=false",
	}, func(wakeConfig) error {
		paths := linuxWakeReloadSocketPathsForTest(t, root, "codex")
		if len(paths) != 0 {
			t.Fatalf("signal-capable Linux wake opened reload endpoints: %#v", paths)
		}

		inspection := inspectWakeLock(root, "codex")
		if inspection.Lock.ResumeSchema != wakeResumeSchemaV2 ||
			!sameWakeOwner(inspection.Lock.ResumeOwner, &owner) ||
			inspection.Lock.ResumeSignal != wakeResumeSignalUSR1 ||
			inspection.Lock.ControlSocket != "" ||
			inspection.Lock.RunningImageEvidence == nil {
			t.Fatalf("Linux signal resume advertisement = %#v", inspection.Lock)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunWakeWithLoopSkipsObsoleteLinuxReloadTransportForSignalWake(t *testing.T) {
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
	oldStart := startWakeReloadTransportForWake
	startWakeReloadTransportForWake = func(
		_ *wakeAgentDir,
		_ string,
		_ string,
		_ wakeLockInspection,
		_ wakeOwner,
	) (func(), error) {
		startCalled = true
		return nil, errors.New("obsolete reload transport must not start")
	}
	t.Cleanup(func() { startWakeReloadTransportForWake = oldStart })

	loopCalled := false
	runErr := runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-mode", wakeInjectModeNone,
		"--interrupt=false",
	}, func(wakeConfig) error {
		loopCalled = true
		if paths := linuxWakeReloadSocketPathsForTest(t, root, "codex"); len(paths) > 0 {
			t.Fatalf("signal wake reload endpoint paths = %#v", paths)
		}
		return nil
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if startCalled || !loopCalled {
		t.Fatalf("reload start called = %v, loop called = %v", startCalled, loopCalled)
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
