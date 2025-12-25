package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteFileAtomic writes data to a temporary file in dir and renames it into place.
func WriteFileAtomic(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpName := fmt.Sprintf(".%s.tmp-%d", filename, time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	finalPath := filepath.Join(dir, filename)

	if err := writeAndSync(tmpPath, data, perm); err != nil {
		return "", err
	}
	if err := SyncDir(dir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// On Windows, rename to existing file may return access denied instead of ErrExist.
		// Check if destination exists and retry after removal.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			if removeErr := os.Remove(finalPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return "", cleanupTemp(tmpPath, err)
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				return "", cleanupTemp(tmpPath, err)
			}
		} else {
			return "", cleanupTemp(tmpPath, err)
		}
	}
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	return finalPath, nil
}

func writeAndSync(path string, data []byte, perm os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func cleanupTemp(path string, primary error) error {
	if primary == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (cleanup: %v)", primary, err)
	}
	return primary
}
