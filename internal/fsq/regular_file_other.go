//go:build !darwin && !linux

package fsq

import "os"

func openRegularNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
