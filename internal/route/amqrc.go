package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Amqrc is the .amqrc configuration file format.
// Root is the literal queue root directory (e.g., ".agent-mail").
// Agent identity ('me') should be set per-terminal via AM_ME env var or --me flag.
type Amqrc struct {
	Root    string            `json:"root"`
	Project string            `json:"project,omitempty"` // explicit project name (defaults to directory basename)
	Peers   map[string]string `json:"peers,omitempty"`   // peer name → peer's base root path
}

// AmqrcFile holds a parsed .amqrc and where it was found.
type AmqrcFile struct {
	Config Amqrc
	Dir    string // Directory containing .amqrc (for resolving relative paths)
	Path   string // Canonical path of the config file (shared provenance)
}

// ErrAmqrcNotFound reports that no eligible .amqrc exists (non-fatal).
var ErrAmqrcNotFound = errors.New(".amqrc not found")

// FindAmqrc searches dir and its eligible ancestors for .amqrc.
// It returns ErrAmqrcNotFound if none exists (non-fatal), and other errors
// for an invalid, unreadable, or untrusted .amqrc (fatal).
func (w Walker) FindAmqrc(dir string) (AmqrcFile, error) {
	ceiling := ""
	if top, insideGit := w.GitBoundary(dir); insideGit {
		ceiling = top
		if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
			dir = resolved
		}
	}

	// ~/.amqrc is a global fallback, not a project config. Loading it here
	// mislabeled the global queue as project-local and let it shadow a repo's
	// own .agent-mail. When HOME is itself the Git worktree root, however, the
	// file is worktree-local authority and the inclusive Git ceiling wins.
	for w.Inspects(dir, ceiling) {
		rcPath := filepath.Join(dir, ".amqrc")
		// Refuse configuration whose provenance cannot be established. In
		// particular, symlinks and group/world-writable files are attacker
		// controlled in common shared-directory setups.
		if info, statErr := os.Lstat(rcPath); statErr == nil {
			if err := ValidateAmqrcInfo(rcPath, info); err != nil {
				return AmqrcFile{}, err
			}
			if err := ValidateAmqrcFile(rcPath); err != nil {
				return AmqrcFile{}, err
			}
		} else if !os.IsNotExist(statErr) {
			return AmqrcFile{}, fmt.Errorf("cannot inspect .amqrc at %s: %w", rcPath, statErr)
		}
		data, err := os.ReadFile(rcPath)
		if err != nil {
			if os.IsNotExist(err) {
				if ceiling != "" && SameCleanPath(dir, ceiling) {
					break
				}
				// Keep searching in eligible parent directories.
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
				continue
			}
			// Other read errors (permissions, etc.) are fatal
			return AmqrcFile{}, fmt.Errorf("cannot read .amqrc at %s: %w", rcPath, err)
		}

		// File exists, try to parse it
		var rc Amqrc
		if err := json.Unmarshal(data, &rc); err != nil {
			return AmqrcFile{}, fmt.Errorf("invalid .amqrc at %s: %w", rcPath, err)
		}
		return AmqrcFile{Config: rc, Dir: dir, Path: rcPath}, nil
	}

	return AmqrcFile{}, ErrAmqrcNotFound
}

// ValidateAmqrcInfo refuses an .amqrc that is a symlink or not a regular
// file.
func ValidateAmqrcInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing untrusted .amqrc at %s: symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing untrusted .amqrc at %s: not a regular file", path)
	}
	return nil
}
