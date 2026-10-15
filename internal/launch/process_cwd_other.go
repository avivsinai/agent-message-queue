//go:build !darwin && !linux

package launch

import "errors"

// ProcessCwd is unavailable: this platform does not expose another
// process's working directory.
func ProcessCwd(int) (string, error) {
	return "", errors.New("process working directory unavailable on this platform")
}
