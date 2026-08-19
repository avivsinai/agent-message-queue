//go:build !unix

package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func (File) Inject(ctx context.Context, target string, payload string) error {
	clean, err := prepareFileInject(ctx, target)
	if err != nil {
		return err
	}
	info, err := os.Lstat(clean)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("file adapter target %q must not be a symlink: %w", clean, ErrTargetSymlink)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("file adapter target %q must be a regular file: %w", clean, ErrTargetNotRegular)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(payload + "\n"); err != nil {
		return err
	}
	return file.Chmod(0o600)
}
