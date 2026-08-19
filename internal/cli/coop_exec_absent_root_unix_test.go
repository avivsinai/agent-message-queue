//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCoopExecAbsentRootRefusesPlantedSymlink(t *testing.T) {
	base := secureTempDirForTest(t)
	victim := filepath.Join(base, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	canary := filepath.Join(victim, "canary")
	if err := os.WriteFile(canary, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	root := filepath.Join(base, "queue")

	oldHook := fsq.AfterCreateDirectoryPathExclusiveLstatNotExistForTest
	fsq.AfterCreateDirectoryPathExclusiveLstatNotExistForTest = func(path string) {
		if path != root {
			return
		}
		if err := os.Symlink(victim, root); err != nil {
			t.Errorf("plant symlink: %v", err)
		}
	}
	t.Cleanup(func() { fsq.AfterCreateDirectoryPathExclusiveLstatNotExistForTest = oldHook })

	execCalled := false
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error {
		execCalled = true
		return errors.New("unexpected exec")
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{"--root", root, "--me", "alice", "--no-wake", "-y", "sh"})
	if !errors.Is(err, fsq.ErrPathIsSymlink) {
		t.Fatalf("coop exec error = %v, want ErrPathIsSymlink", err)
	}
	if execCalled {
		t.Fatal("coop exec replaced the process after refusing a planted symlink")
	}

	got, readErr := os.ReadFile(canary)
	if readErr != nil {
		t.Fatalf("read victim canary: %v", readErr)
	}
	if string(got) != "untouched" {
		t.Fatalf("victim canary = %q, want untouched", got)
	}
	if _, statErr := os.Lstat(filepath.Join(victim, "agents")); !os.IsNotExist(statErr) {
		t.Fatalf("victim gained agents/: %v", statErr)
	}
	info, lstatErr := os.Lstat(root)
	if lstatErr != nil {
		t.Fatalf("lstat planted root: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("absent root became %v, want leftover planted symlink", info.Mode())
	}
}

func TestCoopExecAbsentRootCreatesExclusiveQueue(t *testing.T) {
	root := filepath.Join(secureTempDirForTest(t), "queue")
	sentinel := errors.New("exec sentinel")
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error {
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{"--root", root, "--me", "alice", "--no-wake", "-y", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want exec sentinel", err)
	}

	info, lstatErr := os.Lstat(root)
	if lstatErr != nil {
		t.Fatalf("lstat created root: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("created root mode = %v, want real directory", info.Mode())
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("created root mode = %04o, want 0700", info.Mode().Perm())
	}
	if _, statErr := os.Stat(filepath.Join(root, "agents", "alice", "inbox", "new")); statErr != nil {
		t.Fatalf("created mailbox: %v", statErr)
	}
}
