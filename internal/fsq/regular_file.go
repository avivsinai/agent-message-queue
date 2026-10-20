package fsq

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// OpenRegularNoFollowBounded opens path for reading only if it is a regular
// file, never through a symlink, and still the very file its lstat saw:
//
//  1. Lstat gate: the leaf must exist and be a regular file, and with
//     max > 0 be no larger than max. A symlink, FIFO, directory or huge
//     file is refused before any open.
//  2. On darwin and linux the open is no-follow and non-blocking, so a
//     symlink or FIFO swapped in after the gate is refused or cannot block.
//     Other platforms (Windows) open normally; only step 3 stands there,
//     and a Windows Lstat identity is not reliable, so a caller that needs
//     the no-follow guarantee refuses on those platforms itself.
//  3. The opened file must be a regular file and os.SameFile with the lstat
//     result.
//
// max <= 0 sets no size bound. The lstat error passes through unwrapped, so
// errors.Is(err, os.ErrNotExist) reports a missing file. The caller owns
// the returned file.
func OpenRegularNoFollowBounded(path string, max int64) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: not a regular file (mode %s); refusing", path, info.Mode())
	}
	if max > 0 && info.Size() > max {
		return nil, nil, fmt.Errorf("%s: %d bytes exceeds %d; refusing", path, info.Size(), max)
	}
	if afterRegularLstatForTest != nil {
		afterRegularLstatForTest(path)
	}
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s: replaced between lstat and open; refusing", path)
	}
	return file, openedInfo, nil
}

// afterRegularLstatForTest runs between the lstat gate and the open in
// OpenRegularNoFollowBounded so a test can swap the path.
var afterRegularLstatForTest func(path string)

// ReadRegularNoFollowBounded reads a whole file of at most max bytes through
// OpenRegularNoFollowBounded; max must be positive. A file that grows past
// max between the lstat gate and the read is refused too.
func ReadRegularNoFollowBounded(path string, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("%s: read bound %d is not positive", path, max)
	}
	file, _, err := OpenRegularNoFollowBounded(path, max)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes after read; refusing", path, max)
	}
	return raw, nil
}

// CreateNewFile publishes data at path only if nothing is there yet, with
// its whole content visible at once: the bytes go to a synced temporary file
// in the same directory that is then hard-linked onto path, which fails when
// the name exists. created reports whether this call published the file. In
// both outcomes the directory is synced and its error returned, so a file
// another writer published is durable before the caller acts on it.
func CreateNewFile(path string, data []byte, perm os.FileMode) (created bool, err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := writeAllAndSync(tmp, data); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
	} else {
		created = true
	}
	_ = os.Remove(tmpName)
	return created, SyncDir(dir)
}
