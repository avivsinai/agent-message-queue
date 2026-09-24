//go:build !windows

package binding

import "syscall"

// openNoFollowFlag refuses a symlink leaf and a FIFO swapped in after the
// lstat gate.
const openNoFollowFlag = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

const noFollowSupported = true
