//go:build windows

package cli

import "os"

// Windows has no portable Unix ownership or mode bits to validate.
func validateAmqrcFile(path string) error {
	_, err := os.Stat(path)
	return err
}
