//go:build !darwin && !linux

package codex

import "errors"

func openFilePaths(int) ([]string, error) {
	return nil, errors.New("another process's open files are unavailable on this platform")
}
