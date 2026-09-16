//go:build !windows

package fsq

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// removeSourceExclusively acquires inbox/new/<name> by removing it WITHOUT
// linking into any destination — the SAME disposal the normal claim performs
// after linking new→cur. This is the single shared-ownership decision
// (agent-message-queue-611.22.42, Pro P1-3): exactly one concurrent actor
// (this quarantine OR a normal claimer) wins; every loser observes ENOENT.
// Because acquisition is by disposal (not by linking into a second
// destination), two concurrent quarantines cannot both believe they own the
// source the way two link insertions can.
//
// Returns (true, nil) if this caller removed it; (false, nil) for a clean
// loss (the source is already gone — another claimer won); (false, err) when
// ownership is indeterminate and the caller must fail closed.
func removeSourceExclusively(root *DeliveryRoot, newPath string) (bool, error) {
	if err := root.VerifyBase(); err != nil {
		return false, err
	}
	if err := root.root.Remove(newPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("exclusively remove claim source: %w", err)
	}
	return true, nil
}
