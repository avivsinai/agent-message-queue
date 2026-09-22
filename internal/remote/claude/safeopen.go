package claude

import (
	"fmt"
	"io"
	"os"
)

// openRegular opens a local-writable path for reading with the package's
// fail-closed rules, shared by every reader of a file the target harness
// (or anything with the same uid) can replace under us: the session
// registry, the peer-key file, the transcript, and the Stop marker.
//
//  1. Lstat gate: the leaf must exist and be a regular file. A symlink,
//     FIFO or directory is refused before any open.
//  2. Size bound (maxBytes > 0): checked on the lstat result BEFORE open
//     so a huge file is never mapped or read (r3 P1 on #852).
//  3. Open with openNoFollowFlag: O_NOFOLLOW|O_NONBLOCK on unix. O_NOFOLLOW
//     refuses a symlink swapped in after the lstat; O_NONBLOCK makes an
//     open of a FIFO swapped in after the lstat return at once instead of
//     blocking until a writer appears (codex #855 r1 item 9). Regular
//     files ignore O_NONBLOCK.
//  4. Fstat recheck on the open description: anything but a regular file
//     is closed and refused. On Windows the flags are 0, so this recheck
//     is the only guard there and it detects rather than prevents a
//     blocking open; see registrynofollow_windows.go.
//
// The caller owns the returned file.
func openRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: not a regular file (mode %s); refusing", path, fi.Mode())
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%s: %d bytes exceeds %d; refusing", path, fi.Size(), maxBytes)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollowFlag, 0)
	if err != nil {
		return nil, nil, err
	}
	fi2, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi2.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: replaced by a non-regular file between lstat and open; refusing", path)
	}
	return f, fi2, nil
}

// readRegularBounded reads a whole small file through openRegular. A file
// that grows past maxBytes between lstat and read is refused too
// (LimitReader defense in depth). os.ErrNotExist passes through unwrapped
// so callers can treat a missing file as its own condition.
func readRegularBounded(path string, maxBytes int64) ([]byte, error) {
	f, _, err := openRegular(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes after read; refusing", path, maxBytes)
	}
	return raw, nil
}
