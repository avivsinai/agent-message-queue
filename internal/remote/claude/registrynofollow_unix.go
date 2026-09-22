//go:build !windows

package claude

import "syscall"

// openNoFollowFlag is OR'd into every read-side open of a local-writable
// file (registry, peer key, transcript, Stop marker). O_NOFOLLOW refuses a
// symlink swapped in after the lstat gate; O_NONBLOCK makes the open of a
// FIFO swapped in after the gate return at once (a blocking FIFO open would
// hold the endpoint mutex until a writer appeared — codex #855 r1 item 9).
// Regular files ignore O_NONBLOCK, so reads are unaffected.
const openNoFollowFlag = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// noFollowSupported gates every local-writable file operation; unix has
// O_NOFOLLOW|O_NONBLOCK.
const noFollowSupported = true
