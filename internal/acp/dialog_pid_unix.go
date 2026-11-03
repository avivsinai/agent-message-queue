//go:build !windows

package acp

import (
	"errors"

	"golang.org/x/sys/unix"
)

// pidAlive reports whether a process with pid exists. A process of another
// user exists too (EPERM).
func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
