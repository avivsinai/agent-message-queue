//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"syscall"
)

func wakeLockHasMultipleLinks(info os.FileInfo) (bool, error) {
	if info == nil {
		return false, errors.New("wake lock link count unavailable: file metadata is nil")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return false, errors.New("wake lock link count unavailable: file metadata has no stat")
	}
	return uint64(stat.Nlink) > 1, nil
}
