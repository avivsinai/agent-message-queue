//go:build !windows

package claude

import "syscall"

// openNoFollowFlag is OR'd into the Stop marker's append open; reads of a
// local-writable file go through openRegular. O_NOFOLLOW refuses a symlink
// swapped in after the lstat gate; O_NONBLOCK makes the open of a FIFO
// swapped in after the gate return at once (a blocking FIFO open would hold
// the endpoint mutex until a writer appeared).
const openNoFollowFlag = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// noFollowSupported gates every local-writable file operation; unix has
// O_NOFOLLOW|O_NONBLOCK.
const noFollowSupported = true
