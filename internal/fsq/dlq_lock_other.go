//go:build !darwin && !linux && !freebsd && !windows

package fsq

import (
	"fmt"
	"os"
)

func withExclusiveDLQEnvelopeLock(_ *os.File, _ func() error) error {
	return fmt.Errorf("DLQ envelope locking is unsupported on this platform")
}
