package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

const gitDiagnosticTimeout = 2 * time.Second

func checkLinkedWorktreeLocalHint(root, rootSource string) []opsHint {
	if !worktreeLocalRootSource(rootSource) {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	top, err := gitTopLevel(cwd)
	if err != nil || !pathWithin(top, root) {
		return nil
	}
	gitMarker, err := os.Stat(filepath.Join(top, ".git"))
	if err != nil || !gitMarker.Mode().IsRegular() {
		return nil
	}
	session := validSessionNameForRoot(root)
	if session == "" {
		return nil
	}
	return []opsHint{{
		Code:   "worktree_session_isolation",
		Status: "warn",
		Message: fmt.Sprintf(
			"linked git worktree session %q is per-worktree at %s (root source: %s); use an absolute root in .amqrc or AMQ_GLOBAL_ROOT to share one mailbox across worktrees",
			session, absPath(resolveRoot(root)), rootSource,
		),
	}}
}

func worktreeLocalRootSource(source string) bool {
	switch rootSource(source) {
	case rootSourceAutoDetect:
		return true
	case rootSourceProjectRC:
		result, err := findAndLoadAmqrc()
		return err == nil && result.Config.Root != "" && !filepath.IsAbs(result.Config.Root)
	default:
		return false
	}
}

func checkWorktreeDivergenceHints(root string, agents []string) []opsHint {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	top, err := gitTopLevel(cwd)
	if err != nil {
		return nil
	}

	root = canonicalDiagnosticPath(root)
	session := validSessionNameForRoot(root)
	if session == "" {
		return nil
	}
	base := canonicalDiagnosticPath(baseRootOfForDisplay(root))
	relBase, err := filepath.Rel(top, base)
	if err != nil || relBase == ".." || strings.HasPrefix(relBase, ".."+string(filepath.Separator)) || filepath.IsAbs(relBase) {
		return nil
	}

	worktrees, err := listGitWorktrees(top)
	if err != nil {
		return nil
	}
	caller := strings.TrimSpace(os.Getenv(envMe))
	var hints []opsHint
	for _, worktree := range worktrees {
		candidate := canonicalDiagnosticPath(filepath.Join(worktree, relBase, session))
		if candidate == root || !dirExists(candidate) {
			continue
		}
		peers := fresherPresenceAgents(root, candidate, agents, caller)
		if len(peers) == 0 {
			continue
		}
		hints = append(hints, opsHint{
			Code:   "worktree_divergence",
			Status: "warn",
			Message: fmt.Sprintf(
				"session %q resolves to different roots across git worktrees: current %s, fresher presence for peer(s) %s at %s; use an absolute root in .amqrc or AMQ_GLOBAL_ROOT to share one mailbox intentionally",
				session, root, strings.Join(peers, ","), candidate,
			),
		})
	}
	return hints
}

func validSessionNameForRoot(root string) string {
	session := resolveSessionNameForDisplay(absPath(resolveRoot(root)))
	if session == "" || validateSessionName(session) != nil {
		return ""
	}
	return session
}

func fresherPresenceAgents(currentRoot, candidateRoot string, agents []string, caller string) []string {
	var fresher []string
	for _, agent := range agents {
		if agent == caller {
			continue
		}
		candidateSeen, ok := presenceLastSeen(candidateRoot, agent)
		if !ok {
			continue
		}
		currentSeen, currentOK := presenceLastSeen(currentRoot, agent)
		if currentOK && !candidateSeen.After(currentSeen) {
			continue
		}
		fresher = append(fresher, agent)
	}
	sort.Strings(fresher)
	return fresher
}

func presenceLastSeen(root, agent string) (time.Time, bool) {
	p, err := presence.Read(root, agent)
	if err != nil {
		return time.Time{}, false
	}
	seen, err := time.Parse(time.RFC3339Nano, p.LastSeen)
	return seen, err == nil
}

func gitTopLevel(cwd string) (string, error) {
	output, err := runGitDiagnostic(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(output))
	if top == "" {
		return "", fmt.Errorf("git returned an empty worktree root")
	}
	return canonicalDiagnosticPath(top), nil
}

func listGitWorktrees(cwd string) ([]string, error) {
	output, err := runGitDiagnostic(cwd, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var worktrees []string
	for _, field := range bytes.Split(output, []byte{0}) {
		const prefix = "worktree "
		if !bytes.HasPrefix(field, []byte(prefix)) {
			continue
		}
		path := strings.TrimSpace(string(field[len(prefix):]))
		if path != "" {
			worktrees = append(worktrees, canonicalDiagnosticPath(path))
		}
	}
	return worktrees, nil
}

func runGitDiagnostic(cwd string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitDiagnosticTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", cwd}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = gitDiagnosticEnv()
	return cmd.Output()
}

func gitDiagnosticEnv() []string {
	blocked := map[string]struct{}{
		"GIT_COMMON_DIR": {},
		"GIT_DIR":        {},
		"GIT_WORK_TREE":  {},
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := blocked[key]; !skip {
			env = append(env, entry)
		}
	}
	return env
}

func pathWithin(parent, child string) bool {
	parent = canonicalDiagnosticPath(parent)
	child = canonicalDiagnosticPath(child)
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func canonicalDiagnosticPath(path string) string {
	path = absPath(resolveRoot(path))
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
