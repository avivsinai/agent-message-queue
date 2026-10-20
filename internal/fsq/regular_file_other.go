//go:build !unix

package fsq

import "os"

func openRegularNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
