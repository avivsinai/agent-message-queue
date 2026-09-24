//go:build unix

package requests

import "syscall"

// recordNoFollowFlag is OR'd into every request-record read. O_NOFOLLOW
// refuses a symlink swapped in after the lstat gate; O_NONBLOCK makes the
// open of a FIFO swapped in after the gate return at once. Regular files
// ignore O_NONBLOCK.
const recordNoFollowFlag = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
