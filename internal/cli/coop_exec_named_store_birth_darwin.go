//go:build darwin

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func cursorChatDirectoryBirthTime(metaPath string) (time.Time, bool, error) {
	info, err := os.Stat(filepath.Dir(metaPath))
	if err != nil {
		return time.Time{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Birthtimespec.Sec == 0 && stat.Birthtimespec.Nsec == 0) {
		return time.Time{}, false, nil
	}
	return time.Unix(stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec), true, nil
}
