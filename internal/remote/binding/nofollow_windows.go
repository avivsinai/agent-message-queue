//go:build windows

package binding

const openNoFollowFlag = 0

// noFollowSupported is false: Windows has no no-follow open, so the binding
// refuses rather than read a redirected file.
const noFollowSupported = false
