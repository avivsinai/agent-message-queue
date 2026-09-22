//go:build !windows

package claude

import "syscall"

// registryNoFollow refuses a symlink at the registry leaf at OPEN time:
// it closes the lstat-open race (r3 P2-2) where the file between the gate
// and the open is swapped for a symlink pointing elsewhere (the LimitReader
// bounds what follows, but refusing outright is stricter and correct).
const registryNoFollow = syscall.O_NOFOLLOW

// openNoFollowFlag is the os.OpenFile flag refusing a final-component
// symlink at open time.
const openNoFollowFlag = registryNoFollow
