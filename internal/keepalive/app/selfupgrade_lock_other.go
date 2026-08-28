//go:build !darwin && !linux

package app

func acquireSelfUpgradeStateLock(string) (func() error, error) {
	return func() error { return nil }, nil
}
