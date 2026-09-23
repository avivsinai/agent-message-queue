//go:build !windows

package sharestate

import (
	"os"
	"syscall"
)

// openLeaf opens a key-directory leaf without following a symlink and
// without blocking on a FIFO swapped in after the lstat gate.
func openLeaf(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
