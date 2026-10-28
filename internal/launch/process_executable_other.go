//go:build !darwin && !linux

package launch

import "errors"

// ProcessExecutable is unavailable on this platform.
func ProcessExecutable(int) (string, error) {
	return "", errors.New("process executable unavailable on this platform")
}

// ProcessArgs is unavailable on this platform.
func ProcessArgs(int) ([]string, error) {
	return nil, errors.New("process arguments unavailable on this platform")
}
