package route

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvBaseRoot names the base root of the participating session.
	EnvBaseRoot = "AM_BASE_ROOT"
	// DefaultRootName is the default queue root directory name.
	DefaultRootName = ".agent-mail"
)

// ResolveRoot resolves a root that names an existing queue. A relative root
// not found below cwd falls back to the nearest ancestor that holds it.
func (w Walker) ResolveRoot(raw string) string {
	root := ResolveCreateRoot(raw)
	if root == "" || filepath.IsAbs(root) {
		return root
	}
	cwd, err := os.Getwd()
	if err != nil {
		return root
	}
	if found, ok := w.FindRootInParents(cwd, root); ok {
		return found
	}
	return root
}

// ResolveCreateRoot resolves a root that a command creates. A relative root
// always names a path below cwd; it never resolves to an ancestor queue.
// The result stays relative when cwd holds no such directory yet.
func ResolveCreateRoot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	cleaned := filepath.Clean(raw)
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cleaned
	}
	candidate := filepath.Join(cwd, cleaned)
	if dirExists(candidate) {
		return absPath(candidate)
	}
	return cleaned
}

// FindRootInParents finds relative below startDir or its nearest eligible
// ancestor.
func (w Walker) FindRootInParents(startDir, relative string) (string, bool) {
	ceiling := ""
	if top, insideGit := w.GitBoundary(startDir); insideGit {
		ceiling = top
		if resolved, resolveErr := filepath.EvalSymlinks(startDir); resolveErr == nil {
			startDir = resolved
		}
	}
	dir := startDir
	for !w.StopsAt(dir) {
		candidate := filepath.Join(dir, relative)
		if dirExists(candidate) {
			return absPath(candidate), true
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
	return "", false
}

// isDefaultCoopRoot recognizes the default root by filesystem identity when a
// case-insensitive volume presents an alternate spelling.
func (w Walker) isDefaultCoopRoot(path string) bool {
	if filepath.Base(path) == DefaultRootName {
		return true
	}
	exact := filepath.Join(filepath.Dir(path), DefaultRootName)
	return w.RelateTrees(path, exact) == TreeRelationSame
}

// ClassifyRoot returns the base root for the given root, or "" if it cannot
// be determined. This is the single authoritative function for root classification.
//
// Resolution order:
//  1. AM_BASE_ROOT, when root is a direct session below it
//  2. If root's parent is the default root (.agent-mail), return that parent
//  3. The nearest root-aware .amqrc, when root is the configured base or a direct session below it
//  4. If root itself is the default root name (.agent-mail), return "" (known base)
//  5. Otherwise, return "" (base or unknown — caller must handle)
func (w Walker) ClassifyRoot(root string) string {
	base, _ := w.ClassifyRootWithEvidence(root)
	return base
}

// ClassifyRootWithEvidence distinguishes a known base from an unknown root,
// while preserving ClassifyRoot's empty result for a base root itself.
func (w Walker) ClassifyRootWithEvidence(root string) (base string, known bool) {
	if strings.TrimSpace(root) == "" {
		return "", false
	}
	resolvedRoot := absPath(w.ResolveRoot(root))
	if base := strings.TrimSpace(os.Getenv(EnvBaseRoot)); base != "" {
		base = absPath(w.ResolveRoot(base))
		if resolvedRoot == base {
			return "", true
		}
		if w.IsSessionRootUnderBase(resolvedRoot, base) {
			return base, true
		}
	}
	parent := filepath.Dir(resolvedRoot)
	// A direct child of .agent-mail is always a session; this structural rule
	// deliberately outranks root-local .amqrc and must not be rebased.
	if w.isDefaultCoopRoot(parent) {
		return parent, true
	}
	if base := w.configuredBaseRoot(resolvedRoot); base != "" {
		if resolvedRoot == base {
			return "", true
		}
		return base, true
	}
	if w.isDefaultCoopRoot(resolvedRoot) {
		return "", true
	}
	return "", false
}

func (w Walker) configuredBaseRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	result, err := w.FindAmqrcForRoot(root)
	if err != nil {
		if !errors.Is(err, ErrAmqrcNotFound) {
			_, _ = fmt.Fprintf(os.Stderr, "warning: reading .amqrc for root %q: %v\n", root, err)
		}
		return ""
	}
	if result.Config.Root == "" {
		return ""
	}
	base := result.Config.Root
	if !filepath.IsAbs(base) {
		base = filepath.Join(result.Dir, base)
	}
	base = absPath(base)
	if w.isBaseOrSessionRoot(root, base) {
		return base
	}
	return ""
}

// isBaseOrSessionRoot returns true when root is either the base itself or a
// direct child session below it.
func (w Walker) isBaseOrSessionRoot(root, base string) bool {
	root = strings.TrimSpace(root)
	base = strings.TrimSpace(base)
	if root == "" || base == "" {
		return false
	}
	root = absPath(w.ResolveRoot(root))
	base = absPath(w.ResolveRoot(base))
	return root == base || filepath.Dir(root) == base
}

// IsSessionRootUnderBase returns true when root is a direct child of base
// (i.e., root is a session directory like .agent-mail/collab under .agent-mail).
// Returns false when root == base (the base root itself is not a session).
func (w Walker) IsSessionRootUnderBase(root, base string) bool {
	root = strings.TrimSpace(root)
	base = strings.TrimSpace(base)
	if root == "" || base == "" {
		return false
	}
	root = absPath(w.ResolveRoot(root))
	base = absPath(w.ResolveRoot(base))
	if root == base {
		return false
	}
	return filepath.Dir(root) == base
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
