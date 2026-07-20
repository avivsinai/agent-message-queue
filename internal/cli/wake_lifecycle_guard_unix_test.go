//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeLifecycleGuardIsPermanentAndSerializes(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWakeLifecycleGuard(root, "codex", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	var secondEntered atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withWakeLifecycleGuard(root, "codex", func() error {
			secondEntered.Store(true)
			return nil
		})
	}()
	time.Sleep(25 * time.Millisecond)
	if secondEntered.Load() {
		t.Fatal("second lifecycle mutator entered before the first released the guard")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first withWakeLifecycleGuard: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second withWakeLifecycleGuard: %v", err)
	}

	info, err := os.Stat(wakeLifecycleGuardPath(root, "codex"))
	if err != nil {
		t.Fatalf("permanent lifecycle guard missing: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lifecycle guard mode = %v, want regular 0600", info.Mode())
	}
}

func TestWakeLifecycleGuardRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, wakeLifecycleGuardPath(root, "codex")); err != nil {
		t.Fatalf("symlink lifecycle guard: %v", err)
	}

	err := withWakeLifecycleGuard(root, "codex", func() error {
		t.Fatal("guard callback must not run")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "lifecycle guard") {
		t.Fatalf("expected lifecycle guard symlink rejection, got %v", err)
	}
}

func TestWakeLifecycleGuardRejectsWrongOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	guardPath := wakeLifecycleGuardPath(root, "codex")
	if err := os.WriteFile(guardPath, nil, 0o600); err != nil {
		t.Fatalf("write lifecycle guard: %v", err)
	}
	oldOwner := wakeTargetFileOwnerUID
	wakeTargetFileOwnerUID = func(os.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
	t.Cleanup(func() { wakeTargetFileOwnerUID = oldOwner })

	err := withWakeLifecycleGuard(root, "codex", func() error {
		t.Fatal("guard callback must not run")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("expected lifecycle guard ownership rejection, got %v", err)
	}
}

func TestWakeLifecycleGuardRejectsNonRegularFile(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	guardPath := wakeLifecycleGuardPath(root, "codex")
	if err := syscall.Mkfifo(guardPath, 0o600); err != nil {
		t.Fatalf("mkfifo lifecycle guard: %v", err)
	}

	done := make(chan error, 1)
	var callbackRan atomic.Bool
	go func() {
		done <- withWakeLifecycleGuard(root, "codex", func() error {
			callbackRan.Store(true)
			return nil
		})
	}()
	select {
	case err := <-done:
		if callbackRan.Load() {
			t.Fatal("guard callback ran for non-regular guard")
		}
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("expected non-regular lifecycle guard rejection, got %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("lifecycle guard open blocked on FIFO")
	}
}
