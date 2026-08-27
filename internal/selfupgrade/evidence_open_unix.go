//go:build darwin || linux

package selfupgrade

import (
	"os"
	"syscall"
)

func openImageMetadataFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
