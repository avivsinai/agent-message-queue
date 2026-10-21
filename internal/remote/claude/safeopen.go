package claude

import (
	"errors"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// errUnsupportedPlatform is the typed refusal on a platform without a
// no-follow, non-blocking open (Windows).
var errUnsupportedPlatform = errors.New("claude adapter: unsupported on this platform (no no-follow file open)")

// openRegular opens a local-writable path for reading with
// fsq.OpenRegularNoFollowBounded, shared by every reader of a file the
// target harness (or anything with the same uid) can replace under us: the
// session registry, the peer-key file, the transcript, the Stop marker and
// settings.json. maxBytes <= 0 sets no size bound.
//
// Windows has no no-follow open, so openRegular refuses there outright with
// errUnsupportedPlatform (codex #855 r3 item 2).
//
// The caller owns the returned file.
func openRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error) {
	if !noFollowSupported {
		return nil, nil, errUnsupportedPlatform
	}
	return fsq.OpenRegularNoFollowBounded(path, maxBytes)
}

// readRegularBounded reads a whole small file with
// fsq.ReadRegularNoFollowBounded, refusing on Windows like openRegular.
// os.ErrNotExist passes through unwrapped so callers can treat a missing
// file as its own condition.
func readRegularBounded(path string, maxBytes int64) ([]byte, error) {
	if !noFollowSupported {
		return nil, errUnsupportedPlatform
	}
	return fsq.ReadRegularNoFollowBounded(path, maxBytes)
}
