//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package registry

import (
	"errors"
	"os"
)

func uncertainSymlinkResolution(err error) bool {
	return errors.Is(err, os.ErrPermission)
}
