//go:build !darwin && !linux

package xferv3

func HoldPID(_ string, fn func() error) error {
	return fn()
}
