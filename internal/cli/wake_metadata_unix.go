//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

func openWakeMetadataFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
