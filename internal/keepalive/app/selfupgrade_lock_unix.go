//go:build darwin || linux

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireSelfUpgradeStateLock(statePath string) (func() error, error) {
	if statePath == "" {
		return nil, errors.New("self-upgrade state path is empty")
	}
	lockPath := filepath.Join(filepath.Dir(statePath), selfUpgradeStateLockName)
	fd, err := unix.Open(
		lockPath,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open self-upgrade state lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open self-upgrade state lock file descriptor")
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat self-upgrade state lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("self-upgrade state lock is not a private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock self-upgrade state: %w", err)
	}
	closeOnError = false
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil && closeErr != nil {
			return errors.Join(
				fmt.Errorf("unlock self-upgrade state: %w", unlockErr),
				fmt.Errorf("close self-upgrade state lock: %w", closeErr),
			)
		}
		if unlockErr != nil {
			return fmt.Errorf("unlock self-upgrade state: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close self-upgrade state lock: %w", closeErr)
		}
		return nil
	}, nil
}
