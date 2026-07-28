//go:build darwin || linux || freebsd

package fsq

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func withExclusiveDLQEnvelopeLock(file *os.File, fn func() error) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire DLQ envelope lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	return fn()
}
