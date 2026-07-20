//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

const wakeLifecycleGuardFileName = ".wake.lifecycle.lock"

func wakeLifecycleGuardPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeLifecycleGuardFileName)
}

// Lock order: lifecycle guard -> wake lock/target/ready reads and mutations.
// Release the lifecycle guard before any child wait, pidfd exit wait, or
// control wait. Child/cooperative wake paths reacquire it for exact-generation
// cleanup and final readiness publication. Never wait on a child while holding
// the lifecycle guard.
func withWakeLifecycleGuard(root, me string, fn func() error) error {
	path := wakeLifecycleGuardPath(root, me)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create wake lifecycle guard directory: %w", err)
	}
	file, err := openWakeLifecycleGuard(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire wake lifecycle guard %s: %w", path, err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat wake lifecycle guard %s: %w", path, err)
	}
	if err := validateWakeLifecycleGuard(path, info); err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat wake lifecycle guard path %s: %w", path, err)
	}
	if err := validateWakeLifecycleGuard(path, pathInfo); err != nil {
		return err
	}
	if !os.SameFile(info, pathInfo) {
		return fmt.Errorf("wake lifecycle guard %s changed while acquiring", path)
	}
	return fn()
}

func openWakeLifecycleGuard(path string) (*os.File, error) {
	flags := os.O_RDWR | os.O_CREATE | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open wake lifecycle guard %s: %w", path, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat wake lifecycle guard %s: %w", path, statErr)
	}
	if validateErr := validateWakeLifecycleGuard(path, info); validateErr != nil {
		_ = file.Close()
		return nil, validateErr
	}
	return file, nil
}

func validateWakeLifecycleGuard(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake lifecycle guard %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake lifecycle guard %s mode is %o, want 0600", path, got)
	}
	return validateWakeTargetPathOwnership("wake lifecycle guard", path, info)
}
