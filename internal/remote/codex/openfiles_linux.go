//go:build linux

package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// openFilesMaxFDs bounds the descriptors read from one process.
const openFilesMaxFDs = 4096

// openFilePaths is the path of each file process pid holds open.
func openFilePaths(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid process id %d", pid)
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > openFilesMaxFDs {
		return nil, fmt.Errorf("process %d holds more than %d descriptors", pid, openFilesMaxFDs)
	}
	var paths []string
	for _, e := range entries {
		if path, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && filepath.IsAbs(path) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
