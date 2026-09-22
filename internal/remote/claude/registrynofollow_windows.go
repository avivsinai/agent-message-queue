//go:build windows

package claude

// openNoFollowFlag: Windows os.OpenFile has no O_NOFOLLOW equivalent; the
// open-then-fstat recheck in readTranscriptTail carries the guarantee
// (a symlink swapped in after the lstat is caught by the f.Stat mode
// check on the open description — Windows symlinks resolve at open, and
// the recheck refuses non-regular results).
const openNoFollowFlag = 0

// registryNoFollow: no O_NOFOLLOW on Windows; the open-then-fstat recheck
// in the readers carries the no-symlink-follow guarantee (a swapped
// symlink resolves at open and is refused by the mode recheck).
const registryNoFollow = 0
