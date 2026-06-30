//go:build !windows

package fsq

import "os"

func replaceFile(tmpPath, finalPath string) error {
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if _, statErr := os.Stat(finalPath); statErr == nil {
			if removeErr := os.Remove(finalPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return err
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}
