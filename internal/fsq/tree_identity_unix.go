//go:build !windows

package fsq

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

func platformStableTreeIdentity(_ string, info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem identity unavailable on %s", runtime.GOOS)
	}
	return fmt.Sprintf("v1:%s:%x:%x", runtime.GOOS, uint64(stat.Dev), uint64(stat.Ino)), nil
}
