//go:build darwin || linux

package wakelock

import (
	"os"
	"syscall"
)

// OpenReadOnly opens a wake metadata file without following a final symlink
// or blocking on a special file.
func OpenReadOnly(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
