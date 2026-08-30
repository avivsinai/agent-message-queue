//go:build windows

package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type platformWriterLockInspector struct{}

func (platformWriterLockInspector) Held(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		^uint32(0),
		^uint32(0),
		&overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("LockFileEx: %w", err)
	}
	if err := windows.UnlockFileEx(
		windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), &overlapped,
	); err != nil {
		return false, fmt.Errorf("UnlockFileEx: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}
