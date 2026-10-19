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

// RenameDirectChildNoReplace renames one direct child to another direct-child
// name of this root. An existing destination is never replaced.
func (r *DeliveryRoot) RenameDirectChildNoReplace(oldName, newName string) error {
	for _, name := range []string{oldName, newName} {
		if err := validateDirectChildName(name); err != nil {
			return err
		}
	}
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.renameDirectChildNoReplace(oldName, newName)
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
