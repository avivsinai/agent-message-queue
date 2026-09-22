//go:build windows

package claude

// Windows has no O_NOFOLLOW or O_NONBLOCK on os.OpenFile. The lstat gate
// and the post-open Fstat recheck in openRegular are the only guards here:
// they DETECT a symlink or named pipe swapped in between lstat and open
// (the opened description is checked and refused) but cannot PREVENT the
// open itself from blocking on a pipe with no writer. The Claude Code
// messaging socket this adapter drives is a unix-domain socket, and the
// adapter has only been exercised live on macOS.
const openNoFollowFlag = 0
