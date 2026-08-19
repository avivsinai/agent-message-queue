package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	BoxNew = "new"
	BoxCur = "cur"
)

func FindMessage(root, agent, filename string) (string, string, error) {
	if err := ValidateHandle(agent); err != nil {
		return "", "", err
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return "", "", err
	}
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	curPath := filepath.Join(root, "agents", agent, "inbox", "cur", filename)
	if _, err := os.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

func MoveNewToCur(root *DeliveryRoot, agent, filename string) error {
	if err := ValidateHandle(agent); err != nil {
		return err
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return err
	}
	if err := root.VerifyBase(); err != nil {
		return err
	}
	newPath := filepath.Join("agents", agent, "inbox", "new", filename)
	curDir := filepath.Join("agents", agent, "inbox", "cur")
	curPath := filepath.Join(curDir, filename)
	if err := root.root.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	// claimRename is the exclusive-claim point: exactly one concurrent caller
	// wins; losers observe os.IsNotExist. Windows cannot use os.Root.Rename
	// here — it renames by handle and lets every contender succeed (#485).
	if err := claimRename(root, newPath, curPath); err != nil {
		var residue *claimCommittedResidueError
		if errors.As(err, &residue) {
			// The destination name exists and this caller owns the claim; the
			// leftover source name is reconciled by a later claimer. Same
			// contract as a post-rename sync failure: committed, not failed.
			return &CommittedDurabilityError{
				FinalPath: root.displayPath(curPath),
				Recipient: agent,
				Err:       residue.Err,
			}
		}
		return err
	}

	// The rename is already visible. Sync the destination first so a crash is
	// more likely to preserve one claimed copy than to lose the message, but
	// attempt both directories even if either sync fails. Any failure after the
	// rename is a committed claim with indeterminate durability, not a failed
	// claim that callers may safely ignore or retry.
	var durabilityErr error
	if err := root.syncDir(curDir); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync inbox/cur dir: %w", err))
	}
	if err := root.syncDir(filepath.Dir(newPath)); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync inbox/new dir: %w", err))
	}
	if durabilityErr != nil {
		return &CommittedDurabilityError{
			FinalPath: root.displayPath(curPath),
			Recipient: agent,
			Err:       durabilityErr,
		}
	}
	return nil
}
