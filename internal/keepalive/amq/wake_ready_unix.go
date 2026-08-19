//go:build unix

package amq

import (
	"fmt"
	"os"
	"syscall"
)

func readWakeReadyFile(path string) (wakeReadyMarker, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return wakeReadyMarker{}, err
	}
	if err := validateWakeReadyFile(path, info); err != nil {
		return wakeReadyMarker{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return wakeReadyMarker{}, fmt.Errorf("open wake ready file: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeReadyMarker{}, fmt.Errorf("stat opened wake ready file: %w", err)
	}
	if err := validateWakeReadyFile(path, openedInfo); err != nil {
		return wakeReadyMarker{}, err
	}
	if !os.SameFile(info, openedInfo) {
		return wakeReadyMarker{}, fmt.Errorf("wake ready file %s changed while opening", path)
	}
	data, err := readWakeReadyBytes(file, path)
	if err != nil {
		return wakeReadyMarker{}, err
	}
	return decodeWakeReady(data)
}

func validateWakeReadyFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake ready file %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake ready file %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake ready file %s mode is %o, want 0600", path, got)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("wake ready file %s is owned by uid %d, want current uid %d", path, stat.Uid, os.Geteuid())
	}
	return nil
}
