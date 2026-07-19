package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type File struct{}

func (File) Name() string {
	return "file"
}

func (File) Probe(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target == "" {
		return errors.New("file adapter target is required")
	}
	clean := filepath.Clean(target)
	parent := filepath.Dir(clean)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("target parent is not reachable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("target parent %q is not a directory", parent)
	}
	targetInfo, err := os.Stat(clean)
	if err == nil && targetInfo.IsDir() {
		return fmt.Errorf("target %q is a directory", clean)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (File) Inject(ctx context.Context, target string, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := (File{}).Probe(ctx, target); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Clean(target), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(payload + "\n"); err != nil {
		return err
	}
	return file.Chmod(0o600)
}
