package claude

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HookBinary is the path a hook should run for the program name installed
// beside the running program: StableBinary of SiblingBinary(name).
func HookBinary(name string) (string, error) {
	path, err := SiblingBinary(name)
	if err != nil {
		return "", err
	}
	path, _, err = StableBinary(path, name)
	return path, err
}

// SiblingBinary is the program name beside the running program, its
// symlinks resolved, else the one on PATH.
func SiblingBinary(name string) (string, error) {
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			if path := filepath.Join(filepath.Dir(real), name); installedFile(path) {
				return path, nil
			}
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// StableBinary is the path a hook or a login service should run for the
// program at path, named name: the PATH entry when it is the same file, so
// an upgrade that removes the versioned path keeps it working; else path
// with its symlinks resolved, and a warning that says why. A binary under
// the temporary directory (go run) is refused: it disappears.
func StableBinary(path, name string) (stable, warning string, err error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if rel, err := filepath.Rel(tmp, resolved); err == nil && !strings.HasPrefix(rel, "..") {
			return "", "", fmt.Errorf("%s is a temporary build that disappears; install %s, then run it from there", resolved, name)
		}
	}
	looked, err := exec.LookPath(name)
	if err == nil {
		looked, err = filepath.Abs(looked)
	}
	if err != nil {
		return resolved, fmt.Sprintf("runs %s, and no %s is on PATH", resolved, name), nil
	}
	a, errA := os.Stat(looked)
	b, errB := os.Stat(resolved)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return looked, "", nil
	}
	return resolved, fmt.Sprintf("runs %s, not the %s on PATH (%s)", resolved, name, looked), nil
}
