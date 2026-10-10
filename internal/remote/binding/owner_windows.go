//go:build windows

package binding

import "os"

// ownedByUser accepts every directory: Windows reads no binding at all
// (noFollowSupported is false).
func ownedByUser(os.FileInfo) error { return nil }
