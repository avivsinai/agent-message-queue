//go:build darwin || linux

package fsq

import (
	"os"
	"syscall"
)

func openRegularNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
