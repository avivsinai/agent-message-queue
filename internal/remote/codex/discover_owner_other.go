//go:build !unix

package codex

import "os"

// No uid to compare: a symlinked control socket is refused.
func ownedByCurrentUser(os.FileInfo) bool { return false }
