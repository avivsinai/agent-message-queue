package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// FindAmqrcForRoot locates the .amqrc for the given root.
// When root is provided (non-empty), project lookup is scoped to root's
// ancestors. The global ~/.amqrc is eligible only when it configured that exact
// base root or one of its direct sessions. An empty root searches from the
// current directory.
func (w Walker) FindAmqrcForRoot(root string) (AmqrcFile, error) {
	if root != "" {
		absRoot, absErr := filepath.Abs(root)
		if absErr != nil {
			return AmqrcFile{}, absErr
		}
		ceiling := ""
		if top, insideGit := w.GitBoundary(absRoot); insideGit {
			ceiling = top
			if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
				absRoot = resolved
			}
		}
		dir := absRoot
		for w.Inspects(dir, ceiling) {
			rcPath := filepath.Join(dir, ".amqrc")
			if info, statErr := os.Lstat(rcPath); statErr == nil {
				if err := ValidateAmqrcInfo(rcPath, info); err != nil {
					return AmqrcFile{}, err
				}
				if validateErr := ValidateAmqrcFile(rcPath); validateErr != nil {
					return AmqrcFile{}, validateErr
				}
			} else if !os.IsNotExist(statErr) {
				// Do not walk past an unreadable or otherwise unverifiable config.
				return AmqrcFile{}, fmt.Errorf("cannot inspect .amqrc at %s: %w", rcPath, statErr)
			}
			data, readErr := os.ReadFile(rcPath)
			if readErr == nil {
				var rc Amqrc
				if jsonErr := json.Unmarshal(data, &rc); jsonErr != nil {
					return AmqrcFile{}, fmt.Errorf("invalid .amqrc at %s: %w", rcPath, jsonErr)
				}
				return AmqrcFile{Config: rc, Dir: dir, Path: rcPath}, nil
			}
			if !os.IsNotExist(readErr) {
				// Permission or I/O error — report it, don't mask it.
				return AmqrcFile{}, fmt.Errorf("cannot read .amqrc at %s: %w", rcPath, readErr)
			}
			if ceiling != "" && SameCleanPath(dir, ceiling) {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		globalResult, globalErr := w.LoadGlobalAmqrc()
		if globalErr == nil && strings.TrimSpace(globalResult.Config.Root) != "" {
			globalRoot := globalResult.Config.Root
			if !filepath.IsAbs(globalRoot) {
				globalRoot = filepath.Join(globalResult.Dir, globalRoot)
			}
			if w.isBaseOrSessionRoot(absRoot, globalRoot) {
				return globalResult, nil
			}
		}
		return AmqrcFile{}, ErrAmqrcNotFound
	}
	cwd, err := os.Getwd()
	if err != nil {
		return AmqrcFile{}, err
	}
	return w.FindAmqrc(cwd)
}

// LoadGlobalAmqrc reads ~/.amqrc through w.ReadGlobal. A missing file is
// ErrAmqrcNotFound.
func (w Walker) LoadGlobalAmqrc() (AmqrcFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AmqrcFile{}, ErrAmqrcNotFound
	}
	path := filepath.Join(home, ".amqrc")
	if err := validateAmqrcProvenance(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AmqrcFile{}, ErrAmqrcNotFound
		}
		return AmqrcFile{}, err
	}
	readFile := w.ReadGlobal
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AmqrcFile{}, ErrAmqrcNotFound
		}
		return AmqrcFile{}, fmt.Errorf("cannot read ~/.amqrc at %s: %w", path, err)
	}
	if err := ValidateAmqrcFile(path); err != nil {
		return AmqrcFile{}, err
	}
	var cfg Amqrc
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AmqrcFile{}, fmt.Errorf("invalid ~/.amqrc: %w", err)
	}
	return AmqrcFile{Config: cfg, Dir: home, Path: path}, nil
}

func validateAmqrcProvenance(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return ValidateAmqrcInfo(path, info)
}

// Peer resolves the base root of the peer project in f's peers map.
func (f AmqrcFile) Peer(project string) (string, error) {
	if len(f.Config.Peers) == 0 {
		return "", fmt.Errorf("no peers configured in .amqrc (looking for %q)", project)
	}
	peerPath, ok := f.Config.Peers[project]
	if !ok {
		known := make([]string, 0, len(f.Config.Peers))
		for k := range f.Config.Peers {
			known = append(known, k)
		}
		return "", fmt.Errorf("peer %q not found in .amqrc (known: %v)", project, known)
	}
	abs, err := f.PeerPath(peerPath)
	if err != nil {
		return "", fmt.Errorf("resolve peer path for %q: %w", project, err)
	}
	return abs, nil
}

// PeerPath resolves peerPath relative to the directory of f.
func (f AmqrcFile) PeerPath(peerPath string) (string, error) {
	if !filepath.IsAbs(peerPath) {
		peerPath = filepath.Join(f.Dir, peerPath)
	}
	return filepath.Abs(peerPath)
}

// Project is f's project identity: its explicit project, else the basename
// of its directory. The global ~/.amqrc locates a queue and names no project.
func (f AmqrcFile) Project() string {
	if f.Config.Project != "" {
		return f.Config.Project
	}
	// Only infer project from directory basename for project-local .amqrc,
	// not global ~/.amqrc (which is a queue locator, not a project identity).
	home, _ := os.UserHomeDir()
	if home != "" && f.Dir == home {
		return ""
	}
	return filepath.Base(f.Dir)
}
