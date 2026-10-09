// Package amqhome owns the per-user AMQ home, ~/.amq. Every per-user AMQ
// file lives below it, and no feature joins its own home path: callers ask
// this package for the directory. HOME is the only input; there is no
// override, so a wake and the CLI with the same HOME see the same files.
package amqhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const dirName = ".amq"

// Dir is the per-user AMQ home: ~/.amq.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return Under(home), nil
}

// Under is the AMQ home below home, for a process that is handed its home
// (a hook receiver) instead of resolving its own.
func Under(home string) string {
	return filepath.Join(home, dirName)
}

// EnsureDir returns ~/.amq after making sure it is a directory the user can
// trust: created with mode 0700 when missing, and refused when it is a
// symlink, not a directory, owned by another user, or group/world-writable.
func EnsureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s is not a plain directory; refusing", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s is group/world-writable; refusing", dir)
	}
	if owner, ok := fileOwnerUID(info); ok && owner != os.Geteuid() {
		return "", fmt.Errorf("%s is owned by uid %d, not the current user; refusing", dir, owner)
	}
	return dir, nil
}
