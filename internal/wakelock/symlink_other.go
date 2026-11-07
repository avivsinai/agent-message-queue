//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package wakelock

import (
	"errors"
	"os"
)

func isUncertainSymlinkResolution(err error) bool {
	return errors.Is(err, os.ErrPermission)
}
