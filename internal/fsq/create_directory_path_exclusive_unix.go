//go:build darwin || linux

package fsq

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// AfterCreateDirectoryPathExclusiveLstatNotExistForTest runs after a missing
// path component is observed with fstatat(AT_SYMLINK_NOFOLLOW) and before
// exclusive mkdirat. Tests use it to plant a racing symlink at that path.
var AfterCreateDirectoryPathExclusiveLstatNotExistForTest func(path string)

// CreateDirectoryPathExclusive pins the nearest existing parent with nofollow
// dirfds and exclusive-creates each missing component at 0700. A symlink at a
// created component is refused; the returned DeliveryRoot is pinned to the
// created directory capability rather than the ambient lexical path.
func CreateDirectoryPathExclusive(path string) (*DeliveryRoot, error) {
	if path == "" {
		return nil, fmt.Errorf("directory path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve directory path %q: %w", path, err)
	}
	abs = filepath.Clean(abs)

	var missing []string
	current := abs
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if current == abs {
				if info.Mode()&os.ModeSymlink != 0 {
					return nil, &PathSymlinkError{Path: abs}
				}
				if !info.IsDir() {
					return nil, fmt.Errorf("path %q is not a directory", abs)
				}
			} else if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
				return nil, fmt.Errorf("parent path %q is not a directory", current)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("no existing parent for %q", abs)
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}

	fd, err := openExistingParentDir(current)
	if err != nil {
		return nil, fmt.Errorf("open parent %q: %w", current, err)
	}
	created := current
	for _, name := range missing {
		if name == "" || name == "." || name == ".." {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("invalid path component %q", name)
		}
		created = filepath.Join(created, name)
		next, err := mkdiratExclusiveNofollow(fd, name, created)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}

	root, err := deliveryRootFromDirFD(fd, abs)
	_ = unix.Close(fd)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, err
	}
	return root, nil
}

func openExistingParentDir(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return -1, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	if info.Mode()&os.ModeSymlink == 0 {
		flags |= unix.O_NOFOLLOW
	}
	return unix.Open(path, flags, 0)
}

func mkdiratExclusiveNofollow(parentFD int, name, fullPath string) (int, error) {
	var st unix.Stat_t
	err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if unixStatIsSymlink(st) {
			return -1, &PathSymlinkError{Path: fullPath}
		}
		return -1, fmt.Errorf("path %q already exists", fullPath)
	}
	if !errors.Is(err, unix.ENOENT) && !errors.Is(err, fs.ErrNotExist) {
		return -1, err
	}
	if AfterCreateDirectoryPathExclusiveLstatNotExistForTest != nil {
		AfterCreateDirectoryPathExclusiveLstatNotExistForTest(fullPath)
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, fs.ErrExist) {
			var planted unix.Stat_t
			if statErr := unix.Fstatat(parentFD, name, &planted, unix.AT_SYMLINK_NOFOLLOW); statErr == nil && unixStatIsSymlink(planted) {
				return -1, &PathSymlinkError{Path: fullPath}
			}
		}
		return -1, err
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if isNofollowOpenError(err) {
			return -1, &PathSymlinkError{Path: fullPath}
		}
		return -1, err
	}
	return fd, nil
}

func unixStatIsSymlink(st unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFLNK
}

func isNofollowOpenError(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK)
}

func deliveryRootFromDirFD(fd int, abs string) (*DeliveryRoot, error) {
	root, err := os.OpenRoot("/dev/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, fmt.Errorf("pin created directory %q: %w", abs, err)
	}
	identity, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stat created directory %q: %w", abs, err)
	}
	if !identity.IsDir() {
		_ = root.Close()
		return nil, fmt.Errorf("created path is not a directory: %s", abs)
	}
	return &DeliveryRoot{base: abs, root: root, identity: identity}, nil
}
