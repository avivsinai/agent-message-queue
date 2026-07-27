//go:build !unix

package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

func upgradeDestinationWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o222 == 0 {
		return &os.PathError{Op: "access", Path: path, Err: os.ErrPermission}
	}

	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".amq-update-access-*")
	if err != nil {
		return err
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return err
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove writability probe: %w", err)
	}
	return nil
}
