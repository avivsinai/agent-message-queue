//go:build windows

package fsq

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
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
// On Windows the source is opened for DELETE through the pinned os.Root (so an
// ambient rename of DeliveryRoot.Base cannot redirect the acquisition), then
// removed with POSIX disposition semantics. Returns (true, nil) if this caller
// removed it; (false, nil) for a clean loss; (false, err) when ownership is
// indeterminate and the caller must fail closed.
func removeSourceExclusively(root *DeliveryRoot, newPath string) (bool, error) {
	if err := root.VerifyBase(); err != nil {
		return false, err
	}
	newDir, err := root.root.Open(filepath.Dir(newPath))
	if err != nil {
		return false, fmt.Errorf("open claim source directory: %w", err)
	}
	defer func() { _ = newDir.Close() }()

	source, err := openClaimSource(windows.Handle(newDir.Fd()), filepath.Base(newPath))
	if err != nil {
		if claimTransitionAlreadyDone(err) {
			return false, nil
		}
		return false, fmt.Errorf("open claim source %s: %w", root.displayPath(newPath), windowsClaimError(err))
	}
	defer func() { _ = windows.CloseHandle(source) }()

	if err := removeClaimSource(source); err != nil && !claimTransitionAlreadyDone(err) {
		gone, recheckErr := claimSourceNameGone(root, newPath)
		if !gone {
			return false, &claimCommittedResidueError{Err: errors.Join(windowsClaimError(err), recheckErr)}
		}
	}
	return true, nil
}
