//go:build !windows

package claude

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// bindingsDirState reads bindings/*.json through one directory descriptor,
// opened without following a symlink and never reopened by pathname. Each
// entry is opened relative to that descriptor with no-follow and
// non-blocking flags, and the OPENED descriptor is fstat-checked, so a
// swapped-in symlink is refused and a swapped-in FIFO cannot hang the hook.
// present is true for any binding evidence, including a bindings path that
// is a symlink, a file, or unreadable, and every .json-named entry whatever
// its type. allow is true only for a regular file naming this session.
func bindingsDirState(dir, sessionID string) (allow, present bool) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, !errors.Is(err, unix.ENOENT)
	}
	d := os.NewFile(uintptr(fd), dir)
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		present = true
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		present = true
		efd, err := unix.Openat(fd, e.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(efd), e.Name())
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > bindingMaxBytes {
			_ = f.Close()
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(f, bindingMaxBytes+1))
		_ = f.Close()
		if err == nil && int64(len(raw)) <= bindingMaxBytes && nativeSessionIs(raw, sessionID) {
			return true, true
		}
	}
	return false, present
}
