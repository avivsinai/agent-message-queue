package claude

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errUnsupportedPlatform is the typed refusal on a platform without a
// no-follow, non-blocking open (Windows).
var errUnsupportedPlatform = errors.New("claude adapter: unsupported on this platform (no no-follow file open)")

// openRegular opens a local-writable path for reading with the package's
// fail-closed rules, shared by every reader of a file the target harness
// (or anything with the same uid) can replace under us: the session
// registry, the peer-key file, the transcript, the Stop marker and
// settings.json.
//
//  1. Lstat gate: the leaf must exist and be a regular file. A symlink,
//     FIFO or directory is refused before any open.
//  2. Size bound (maxBytes > 0): checked on the lstat result BEFORE open
//     so a huge file is never read (r3 P1 on #852).
//  3. Open with openNoFollowFlag: O_NOFOLLOW|O_NONBLOCK on unix, so a
//     symlink swapped in after the gate is refused and a FIFO swapped in
//     after the gate cannot block the open (codex #855 r1 item 9).
//  4. Same-file check: the opened description must be a regular file AND
//     the very file the lstat gate saw (os.SameFile), defense in depth
//     behind the no-follow open.
//
// Windows has no such open, so openRegular refuses there outright with
// errUnsupportedPlatform (codex #855 r3 item 2).
//
// The caller owns the returned file.
func openRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error) {
	if !noFollowSupported {
		return nil, nil, errUnsupportedPlatform
	}
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
	if !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: replaced between lstat and open; refusing", path)
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
