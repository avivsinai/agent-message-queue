//go:build !windows

package fsq

import (
	"errors"
	"fmt"
	"os"
)

// claimRename is the exclusive-claim primitive behind MoveNewToCur. On POSIX,
// rename(2) resolves the source by path atomically: exactly one of several
// concurrent claimers moves new/<name> to cur/<name> and every loser observes
// ENOENT. The destination is published with no-replace semantics so a
// pre-existing cur/<name> is never overwritten; that collision is reported as
// ClaimCollisionError and both copies survive.
//
// The Windows implementation must provide the same winner/loser contract
// without path-atomic rename: see claim_rename_windows.go and issue #485.
func claimRename(root *DeliveryRoot, newPath, curPath string) error {
	err := root.renameNoReplace(newPath, curPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	if _, statErr := root.lstat(newPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return os.ErrNotExist
		}
		// 611.22.42: a non-ENOENT Lstat (EACCES, EIO, ESTALE) is not a clean
		// loss and not a recoverable collision — the next tick re-claims the
		// same bytes and fails the same way. Classify as permanent so the
		// carrier DLQs the message instead of looping in new forever.
		return &PermanentClaimError{Err: fmt.Errorf("inspect claim source after collision: %w", statErr)}
	}
	return &ClaimCollisionError{
		NewPath: root.displayPath(newPath),
		CurPath: root.displayPath(curPath),
	}
}
