package fsq

import (
	"fmt"
	"io"
	"os"
)

// OpenRegularNoFollow opens path only if it is a regular file and not a symlink.
func OpenRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRegularNoFollowFile(path, info); err != nil {
		return nil, nil, err
	}

	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("stat queue artifact %s: %w", path, err)
	}
	if err := validateRegularNoFollowFile(path, openedInfo); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, openedInfo, nil
}

func ReadRegularNoFollow(path string) ([]byte, error) {
	file, _, err := OpenRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

func validateRegularNoFollowFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("queue artifact %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("queue artifact %s must be a regular file", path)
	}
	return nil
}
