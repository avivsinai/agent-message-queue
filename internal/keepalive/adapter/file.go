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

// NormalizeTarget resolves a file target to its stable absolute pathname.
// Registration persists this value, so a later launchd invocation cannot
// reinterpret a relative target from a different working directory. Resolving
// existing symlinks also keeps lexical and symlink aliases from claiming the
// same destination independently.
func (File) NormalizeTarget(target string) (string, error) {
	if target == "" {
		return "", errors.New("file adapter target is required")
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve file adapter target: %w", err)
	}
	absolute = filepath.Clean(absolute)

	_, err = os.Lstat(absolute)
	if err == nil {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", fmt.Errorf("resolve file adapter target symlink: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", fmt.Errorf("inspect resolved file adapter target: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("file adapter target %q is a directory", resolved)
		}
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect file adapter target: %w", err)
	}
	// The destination may not exist yet, but every existing parent component
	// must resolve uniquely. This makes a new file below a symlinked directory
	// canonical before its first injection.
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve file adapter target parent: %w", err)
	}
	parent = filepath.Clean(parent)
	info, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect file adapter target parent: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("file adapter target parent %q is not a directory", parent)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func (File) Probe(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := (File{}).NormalizeTarget(target)
	if err != nil {
		return err
	}
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
	clean, err := (File{}).NormalizeTarget(target)
	if err != nil {
		return err
	}
	if err := (File{}).Probe(ctx, clean); err != nil {
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
