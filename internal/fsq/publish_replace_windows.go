//go:build windows

package fsq

import (
	"fmt"
	"os"
	"path/filepath"
)

// publishReplaceDurable renames tmpPath onto finalPath (both root-relative,
// replace-existing) durably on Windows: MoveFileEx with MOVEFILE_WRITE_THROUGH,
// the platform's documented rename-durability mechanism (bead u35 architect
// ruling 2026-09-22; a directory flush is not part of the Windows contract).
// The root-relative paths are validated through the pinned os.Root first, so
// an escaping path is refused before any absolute-path rename happens.
func (r *DeliveryRoot) publishReplaceDurable(tmpPath, finalPath string) error {
	// Validate both paths against the pinned root (Root.Stat rejects any
	// path that escapes the root).
	if _, err := r.root.Stat(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("publish destination dir %s escapes the pinned root: %w", finalPath, err)
	}
	if _, err := r.root.Lstat(finalPath); err == nil {
		// Existing destination is fine for replace semantics.
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect publish destination %s: %w", finalPath, err)
	}
	absTmp := filepath.Join(r.root.Name(), filepath.FromSlash(tmpPath))
	absFinal := filepath.Join(r.root.Name(), filepath.FromSlash(finalPath))
	return replaceFile(absTmp, absFinal)
}
