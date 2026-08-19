//go:build !unix

package amq

import (
	"fmt"
	"os"
)

func readWakeReadyFile(path string) (wakeReadyMarker, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return wakeReadyMarker{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return wakeReadyMarker{}, fmt.Errorf("wake ready file %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return wakeReadyMarker{}, fmt.Errorf("wake ready file %s must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return wakeReadyMarker{}, fmt.Errorf("open wake ready file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := readWakeReadyBytes(file, path)
	if err != nil {
		return wakeReadyMarker{}, err
	}
	return decodeWakeReady(data)
}
