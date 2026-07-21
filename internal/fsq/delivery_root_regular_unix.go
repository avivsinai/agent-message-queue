//go:build darwin || linux

package fsq

import (
	"os"
	"syscall"
)

func openRegularNoFollowRoot(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
