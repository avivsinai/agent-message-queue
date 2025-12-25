//go:build windows

package fsq

// SyncDir is a no-op on Windows.
func SyncDir(_ string) error {
	return nil
}
