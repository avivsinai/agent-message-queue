//go:build linux

package cli

import (
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
}
