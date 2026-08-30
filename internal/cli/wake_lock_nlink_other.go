//go:build !darwin && !linux

package cli

import (
	"errors"
	"os"
)

func wakeLockHasMultipleLinks(os.FileInfo) (bool, error) {
	return false, errors.New("wake lock link count unavailable on this platform")
}
