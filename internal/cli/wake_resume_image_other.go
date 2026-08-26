//go:build !darwin

package cli

import "os"

// sameWakeImageStableAcrossHash falls back to the shared file-identity check on
// non-Darwin platforms. The ctime-only hardlink race that motivates a separate
// guard is Darwin-specific (the restart-stage hardlink protocol); elsewhere the
// shared sameWakeFileIdentity, which includes ctime, remains correct.
func sameWakeImageStableAcrossHash(before, after os.FileInfo) bool {
	return sameWakeFileIdentity(before, after) && before.Size() == after.Size()
}
