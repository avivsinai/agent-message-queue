//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

func openWakeMetadataTempFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}
