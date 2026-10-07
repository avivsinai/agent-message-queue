//go:build windows

package codexidentity

// Windows has no O_NOFOLLOW; readStrict still refuses a non-regular file
// and checks the opened file against the one it inspected.
const openNoFollow = 0
