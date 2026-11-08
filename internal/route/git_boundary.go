package route

import (
	"os"
	"path/filepath"
	"strings"
)

// GitBoundary returns the innermost Git worktree or bare-repository boundary
// that contains path. A nested or linked worktree is its own ceiling, so a
// parent checkout's .amqrc or .agent-mail is not inferred.
func (w Walker) GitBoundary(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	path = abs
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	bareCandidate := ""
	for dir := path; ; dir = filepath.Dir(dir) {
		if w.StopsAt(dir) {
			if bareCandidate != "" {
				return bareCandidate, true
			}
			return "", false
		}
		marker := filepath.Join(dir, ".git")
		_, statErr := os.Lstat(marker)
		if statErr == nil {
			return dir, true
		}
		if !os.IsNotExist(statErr) {
			return dir, true
		}
		if bareCandidate == "" && bareGitRepositorySignal(dir) {
			bareCandidate = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if bareCandidate != "" {
				return bareCandidate, true
			}
			return "", false
		}
	}
}

func bareGitRepositorySignal(dir string) bool {
	// A worktree's .git directory has the same internal shape as a bare
	// repository, but it is not the routing boundary. Continue upward so the
	// parent worktree's .git marker wins.
	if filepath.Base(dir) == ".git" {
		return false
	}

	head := bareGitHEADSignal(filepath.Join(dir, "HEAD"))
	objects := bareGitTypedMarkerSignal(filepath.Join(dir, "objects"), true)
	refs := bareGitReferenceStoreSignal(dir)

	// Absence or a wrong marker type disproves the candidate. Once the other
	// repository structure is present, an inspection failure remains a safety
	// signal so permission errors cannot re-enable an implicit home route.
	return head != bareGitMarkerAbsent &&
		objects != bareGitMarkerAbsent &&
		refs != bareGitMarkerAbsent
}

type bareGitMarkerSignal uint8

const (
	bareGitMarkerAbsent bareGitMarkerSignal = iota
	bareGitMarkerPresent
	bareGitMarkerUncertain
)

func bareGitHEADSignal(path string) bareGitMarkerSignal {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bareGitMarkerAbsent
		}
		return bareGitMarkerUncertain
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return bareGitMarkerAbsent
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return bareGitMarkerUncertain
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(head, "ref: "))
		if ref != "" && !strings.ContainsAny(ref, " \t\r\n") {
			return bareGitMarkerPresent
		}
		return bareGitMarkerAbsent
	}
	if len(head) != 40 && len(head) != 64 {
		return bareGitMarkerAbsent
	}
	for _, char := range head {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return bareGitMarkerAbsent
		}
	}
	return bareGitMarkerPresent
}

func bareGitTypedMarkerSignal(path string, directory bool) bareGitMarkerSignal {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bareGitMarkerAbsent
		}
		return bareGitMarkerUncertain
	}
	if directory {
		if info.IsDir() {
			return bareGitMarkerPresent
		}
		return bareGitMarkerAbsent
	}
	if info.Mode().IsRegular() {
		return bareGitMarkerPresent
	}
	return bareGitMarkerAbsent
}

func bareGitReferenceStoreSignal(dir string) bareGitMarkerSignal {
	markers := []struct {
		name      string
		directory bool
	}{
		{name: "refs", directory: true},
		{name: "packed-refs"},
		{name: "reftable", directory: true},
	}
	uncertain := false
	for _, marker := range markers {
		switch bareGitTypedMarkerSignal(filepath.Join(dir, marker.name), marker.directory) {
		case bareGitMarkerPresent:
			return bareGitMarkerPresent
		case bareGitMarkerUncertain:
			uncertain = true
		}
	}
	if uncertain {
		return bareGitMarkerUncertain
	}
	return bareGitMarkerAbsent
}
