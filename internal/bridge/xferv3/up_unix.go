//go:build darwin || linux

package xferv3

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

func HoldPID(root string, fn func() error) error {
	path := pidPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	ok, err := lock.TryLock(file)
	if err != nil {
		return fmt.Errorf("up pidfile: %w", err)
	}
	if !ok {
		return ErrUpRunning
	}
	defer func() { _ = lock.Unlock(file) }()
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return fn()
}
