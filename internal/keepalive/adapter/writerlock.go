package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// WriterLockInspector reports whether a Codex thread-writer lock is held
// WITHOUT acquiring it. The idle-thread case is an untested follow-up; this
// seat only claims submitted delivery when a writer already holds the lock.
type WriterLockInspector interface {
	Held(ctx context.Context, path string) (bool, error)
}

func lsofLockHeld(ctx context.Context, runner CommandRunner, path string) (bool, error) {
	out, err := runner.Run(ctx, "lsof", "-t", path)
	if len(bytes.TrimSpace(out)) > 0 {
		return true, nil
	}
	if err != nil {
		// lsof exits 1 with empty stdout when nobody holds the file. That is
		// idle, not an inspect failure. Any other failure with empty stdout is
		// still ambiguous, so fail closed.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("lsof writer lock %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return false, nil
}
