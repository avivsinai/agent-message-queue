//go:build !unix

package requests

// recordNoFollowFlag is zero where the platform has no no-follow open.
// The lstat gate still rejects a non-regular file and an oversized record
// before any read.
const recordNoFollowFlag = 0
