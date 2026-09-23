//go:build windows

package sharestate

import (
	"fmt"
	"os"
	"path/filepath"
)

// rootDir is a key directory held as an os.Root: opens relative to it
// cannot escape it, even through a component replaced after the check.
type rootDir struct{ r *os.Root }

// openKeyDir walks root to parts, refusing a component that is a symlink or
// junction, and holds each level as an os.Root.
func openKeyDir(root string, parts []string) (dirHandle, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		fi, err := r.Lstat(part)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			_ = r.Close()
			return nil, fmt.Errorf("%s is not a real directory; refusing", path)
		}
		next, err := r.OpenRoot(part)
		_ = r.Close()
		if err != nil {
			return nil, err
		}
		r = next
	}
	return rootDir{r}, nil
}

func (d rootDir) openLeaf(name string) (*os.File, error) {
	fi, err := d.r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return d.r.Open(name)
}

func (d rootDir) Close() error { return d.r.Close() }
