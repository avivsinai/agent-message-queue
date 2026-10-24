package usage

import (
	"bufio"
	"bytes"
	"path/filepath"
	"strings"
)

// Worktrees lists the worktree paths of `git worktree list --porcelain`
// output.
func Worktrees(porcelain []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(porcelain))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "worktree "); ok && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ProjectEligible accepts a working directory that is one of the project's
// roots (its worktrees, or the project directory outside Git) or lies
// inside one, compared after resolving symlinks.
func ProjectEligible(roots []string) Eligible {
	var canon []string
	for _, r := range roots {
		canon = append(canon, canonical(r))
	}
	return func(cwd string) bool {
		c := canonical(cwd)
		for _, r := range canon {
			if c == r || strings.HasPrefix(c, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
}

func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Clean(p)
}
