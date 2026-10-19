package fsq

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lstat stats a root-relative path through the pinned capability without
// following a final symlink.
func (r *DeliveryRoot) Lstat(name string) (os.FileInfo, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	return r.root.Lstat(name)
}

// RenameNoReplace moves a root-relative path to another root-relative path,
// possibly in another directory, without replacing an existing destination
// where the platform has a no-replace primitive.
func (r *DeliveryRoot) RenameNoReplace(oldPath, newPath string) error {
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.renameNoReplace(oldPath, newPath)
}

// RemoveAllDirectChild removes one direct child and everything below it.
// Symlinks inside the tree are removed, never followed.
func (r *DeliveryRoot) RemoveAllDirectChild(name string) error {
	if err := validateDirectChildName(name); err != nil {
		return err
	}
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.root.RemoveAll(name)
}

func validateDirectChildName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid direct child name %q", name)
	}
	return nil
}
