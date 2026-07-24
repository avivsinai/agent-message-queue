//go:build !darwin && !linux

package fsq

import "os"

func openRegularNoFollowRoot(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
