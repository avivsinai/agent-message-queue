//go:build unix

package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (File) Inject(ctx context.Context, target string, payload string) error {
	if _, err := prepareFileInject(ctx, target); err != nil {
		return err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve file adapter target: %w", err)
	}
	abs = filepath.Clean(abs)
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return fmt.Errorf("resolve file adapter target parent: %w", err)
	}
	return injectAt(ctx, filepath.Clean(parent), filepath.Base(abs), payload)
}

func injectAt(ctx context.Context, parent, name, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(parent, name)
	dirfd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open file adapter target parent %q: %w", parent, err)
	}
	defer func() { _ = unix.Close(dirfd) }()

	fd, err := openFileAdapterTarget(dirfd, name, path)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("wrap file adapter target %q", path)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(payload + "\n"); err != nil {
		return err
	}
	return nil
}

func openFileAdapterTarget(dirfd int, name, path string) (int, error) {
	flags := unix.O_WRONLY | unix.O_APPEND | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(dirfd, name, flags, 0)
	if err == unix.ENOENT {
		fd, err = unix.Openat(dirfd, name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	}
	if err != nil {
		if err == unix.ELOOP {
			return -1, fmt.Errorf("file adapter target %q must not be a symlink: %w", path, ErrTargetSymlink)
		}
		if err == unix.ENXIO {
			return -1, fmt.Errorf("file adapter target %q must be a regular file: %w", path, ErrTargetNotRegular)
		}
		return -1, fmt.Errorf("open file adapter target %q: %w", path, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("stat file adapter target %q: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("file adapter target %q must be a regular file: %w", path, ErrTargetNotRegular)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("file adapter target %q is owned by uid %d, want %d", path, stat.Uid, os.Geteuid())
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("chmod file adapter target %q: %w", path, err)
	}
	return fd, nil
}
