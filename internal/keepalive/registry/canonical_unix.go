//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package registry

import (
	"errors"
	"strings"
	"syscall"
)

func uncertainSymlinkResolution(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ELOOP) || strings.Contains(strings.ToLower(err.Error()), "too many links")
}
