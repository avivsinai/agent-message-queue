// Package route discovers the project and Git boundaries that a route starts
// from.
package route

import (
	"os"
	"path/filepath"
	"strings"
)

// Walker walks from a directory toward the filesystem root. Every
// cwd-ancestor resolution walk checks StopsAt before it inspects a directory.
type Walker struct {
	// Stop is a hard boundary whose own entries and ancestors are never
	// inspected. Production leaves it empty, so walks stop at the home
	// directory, the Git ceiling, or the filesystem root. A test that moves
	// HOME off the walk's ancestor chain sets it to keep the walk inside the
	// isolated home.
	Stop string
	// ReadGlobal reads ~/.amqrc; nil means os.ReadFile. A test that proves
	// a resolution never consults the global file sets it.
	ReadGlobal func(path string) ([]byte, error)
}

// StopsAt reports whether a walk must stop before inspecting dir.
func (w Walker) StopsAt(dir string) bool {
	return w.Stop != "" && SameCleanPath(dir, w.Stop)
}

// Inspects reports whether a walk that stops at the home directory may
// inspect dir. Inside a Git worktree (gitCeiling non-empty) the inclusive Git
// ceiling replaces the home stop.
func (w Walker) Inspects(dir, gitCeiling string) bool {
	if w.StopsAt(dir) {
		return false
	}
	return gitCeiling != "" || !IsHomeDir(dir)
}

// SameCleanPath reports whether two paths name the same absolute, cleaned
// path. It does not resolve symlinks.
func SameCleanPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

// IsHomeDir reports whether dir is the user's home directory, by file
// identity when both exist and by resolved path otherwise.
func IsHomeDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return false
	}
	if dirInfo, dirErr := os.Stat(dir); dirErr == nil {
		if homeInfo, homeErr := os.Stat(home); homeErr == nil {
			return os.SameFile(dirInfo, homeInfo)
		}
	}
	if resolvedDir, dirErr := filepath.EvalSymlinks(dir); dirErr == nil {
		dir = resolvedDir
	}
	if resolvedHome, homeErr := filepath.EvalSymlinks(home); homeErr == nil {
		home = resolvedHome
	}
	absDir, dirErr := filepath.Abs(dir)
	absHome, homeErr := filepath.Abs(home)
	if dirErr != nil || homeErr != nil {
		return filepath.Clean(dir) == filepath.Clean(home)
	}
	return filepath.Clean(absDir) == filepath.Clean(absHome)
}
