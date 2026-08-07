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

func TestRunWakeWithLoopClassifiesLegacyLinuxReloadTransportWhenResumeEvidenceUnavailable(t *testing.T) {
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

	oldCapture := captureCurrentWakeImageEvidence
	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) {
		return wakeImageEvidenceV1{}, errors.New("test wake image unavailable")
	}
	t.Cleanup(func() { captureCurrentWakeImageEvidence = oldCapture })

	bindFailure := errors.New("test reload bind failure")
	authorityFailure := errors.New("wake reload owner exited during startup")
	tests := []struct {
		name            string
		startErr        error
		wantErr         error
		wantErrContains string
		wantLoop        bool
		wantDegrade     bool
	}{
		{
			name:        "unavailable degrades",
			startErr:    &wakeReloadTransportUnavailableError{err: bindFailure},
			wantLoop:    true,
			wantDegrade: true,
		},
		{
			name:     "authority failure aborts",
			startErr: authorityFailure,
			wantErr:  authorityFailure,
		},
		{
			name:            "nil cleanup aborts",
			wantErrContains: "wake reload transport returned no cleanup",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startCalls := 0
			oldStart := startWakeReloadTransportForWake
			startWakeReloadTransportForWake = func(
				_ *wakeAgentDir,
				gotRoot string,
				gotAgent string,
				inspection wakeLockInspection,
				gotOwner wakeOwner,
			) (func(), error) {
				startCalls++
				if gotRoot != canonicalWakeRoot(root) || gotAgent != "codex" || gotOwner != owner {
					t.Fatalf("reload start identity = root %q agent %q owner %#v", gotRoot, gotAgent, gotOwner)
				}
				if !inspection.IdentityConfirmed {
					t.Fatal("reload start did not receive confirmed lock identity")
				}
				if inspection.Lock.ResumeSchema != 0 || inspection.Lock.ResumeOwner != nil ||
					inspection.Lock.ResumeSignal != "" || inspection.Lock.ControlSocket != "" ||
					inspection.Lock.RunningImageEvidence != nil {
					t.Fatalf("missing-image fallback lock advertised resume metadata: %#v", inspection.Lock)
				}
				return nil, test.startErr
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
					return nil
				})
			})

			if startCalls != 1 {
				t.Fatalf("reload transport start calls = %d, want 1", startCalls)
			}
			if loopCalled != test.wantLoop {
				t.Fatalf("wake loop called = %v, want %v", loopCalled, test.wantLoop)
			}
			switch {
			case test.wantErr != nil:
				if !errors.Is(runErr, test.wantErr) {
					t.Fatalf("runWakeWithLoop error = %v, want %v", runErr, test.wantErr)
				}
			case test.wantErrContains != "":
				if runErr == nil || !strings.Contains(runErr.Error(), test.wantErrContains) {
					t.Fatalf("runWakeWithLoop error = %v, want containing %q", runErr, test.wantErrContains)
				}
			case runErr != nil:
				t.Fatal(runErr)
			}
			if test.wantDegrade {
				if !strings.Contains(stderr, "reload transport unavailable: "+bindFailure.Error()) ||
					!strings.Contains(stderr, "continuing without reload transport") {
					t.Fatalf("reload transport degradation note = %q", stderr)
				}
			} else if strings.Contains(stderr, "continuing without reload transport") {
				t.Fatalf("hard reload transport failure was narrated as degradable: %q", stderr)
			}
		})
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
