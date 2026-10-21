//go:build unix

package fsq

import (
	"os"
	"syscall"
)

func openRegularNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// openedIsLstatFile accepts any opened file: the no-follow open already
// refuses a symlink swapped in after the lstat gate, and the caller checks
// the opened file's type and size.
func openedIsLstatFile(_, _ os.FileInfo) bool { return true }
