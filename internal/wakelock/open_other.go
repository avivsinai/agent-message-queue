//go:build !darwin && !linux

package wakelock

import "os"

// OpenReadOnly opens a wake metadata file for reading.
func OpenReadOnly(path string) (*os.File, error) {
	return os.Open(path)
}
