package core

import (
	"os"
	"path/filepath"
	"strings"
)

// PathsOverlap reports whether one of two absolute paths is the other or
// lies inside it. Each path is compared in each of its forms, as written
// and with its symbolic links resolved, so a link in any component is
// seen; and by file identity, so a spelling in another letter case on a
// case-insensitive file system is seen. A path that does not exist is
// compared as written.
func PathsOverlap(a, b string) bool {
	for _, x := range pathForms(a) {
		for _, y := range pathForms(b) {
			if within(x, y) || within(y, x) {
				return true
			}
		}
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	return sameFileWithin(a, b) || sameFileWithin(b, a)
}

// pathForms is p cleaned and, when it resolves, p with its symbolic links
// resolved.
func pathForms(p string) []string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		return []string{p, r}
	}
	return []string{p}
}

// sameFileWithin reports whether dir exists and p, or an ancestor of p, is
// the same file as dir.
func sameFileWithin(p, dir string) bool {
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for {
		if pi, err := os.Stat(p); err == nil && os.SameFile(pi, di) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// within reports whether path p is dir or lies inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
