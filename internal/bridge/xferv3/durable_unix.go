//go:build linux

package xferv3

import "os"

func fullSync(f *os.File) error {
	return f.Sync()
}
