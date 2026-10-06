package cli

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// codexTrustsDir reports, read only, whether Codex already trusts dir the way
// codex-cli 0.160 decides it (config/src/config_toml.rs get_active_project,
// config/src/project_trust.rs): the first [projects] key that matches the
// directory, canonical spelling before the original, then the git trust root
// the same way, selects the project, and only trust_level "trusted" counts.
// Only the user config.toml is read; trust that Codex finds in another layer
// reads as untrusted, which keeps the original launch path.
func codexTrustsDir(codexHome, dir string) bool {
	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		return false
	}
	var config struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if toml.Unmarshal(raw, &config) != nil {
		return false
	}
	keys := codexTrustKeys(dir)
	if root := codexGitTrustRoot(dir); root != "" {
		keys = append(keys, codexTrustKeys(root)...)
	}
	for _, key := range keys {
		if project, ok := config.Projects[key]; ok {
			return project.TrustLevel == "trusted"
		}
	}
	return false
}

func codexTrustKeys(path string) []string {
	if canonical, err := filepath.EvalSymlinks(path); err == nil && canonical != path {
		return []string{canonical, path}
	}
	return []string{path}
}

// codexGitTrustRoot is the repository root Codex uses for trust
// (git-utils/src/trust.rs resolve_root_git_project_for_trust): the nearest
// checkout, and for a linked worktree the main checkout that owns it. It is
// "" when Codex would find none.
func codexGitTrustRoot(cwd string) string {
	base := cwd
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		base = filepath.Dir(cwd)
	}
	var root string
	for {
		candidate := codexNearestGitAncestor(base)
		if candidate == "" {
			return ""
		}
		dotGit := filepath.Join(candidate, ".git")
		info, err := os.Stat(dotGit)
		if err != nil {
			return ""
		}
		if _, headErr := os.Stat(filepath.Join(dotGit, "HEAD")); !info.IsDir() || headErr == nil {
			root = candidate
			break
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return ""
		}
		base = parent
	}
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return root
	}
	if !codexGitMetadataFile(dotGit) {
		return ""
	}
	gitDir := codexReadGitdirFile(dotGit)
	if gitDir == "" {
		return ""
	}
	if linfo, err := os.Lstat(gitDir); err != nil || linfo.Mode()&os.ModeSymlink != 0 || !linfo.IsDir() {
		return ""
	}
	canonicalGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return ""
	}
	worktreesDir := filepath.Dir(canonicalGitDir)
	if filepath.Base(worktreesDir) != "worktrees" {
		return ""
	}
	commonDir := filepath.Dir(worktreesDir)
	worktreeGitdir := codexReadGitMetadata(filepath.Join(canonicalGitDir, "gitdir"))
	commondir := codexReadGitMetadata(filepath.Join(canonicalGitDir, "commondir"))
	if worktreeGitdir == "" || commondir == "" {
		return ""
	}
	worktreeDotGit := codexJoinNative(canonicalGitDir, worktreeGitdir)
	if filepath.Base(worktreeDotGit) != ".git" {
		return ""
	}
	registered, err1 := filepath.EvalSymlinks(filepath.Dir(worktreeDotGit))
	checkout, err2 := filepath.EvalSymlinks(root)
	linkedCommon, err3 := filepath.EvalSymlinks(codexJoinNative(canonicalGitDir, commondir))
	if err1 != nil || err2 != nil || err3 != nil || registered != checkout || linkedCommon != commonDir {
		return ""
	}
	mainRoot := filepath.Dir(filepath.Dir(filepath.Dir(gitDir)))
	mainDotGit := filepath.Join(mainRoot, ".git")
	mainInfo, err := os.Stat(mainDotGit)
	if err != nil {
		return ""
	}
	mainGitDir := mainDotGit
	if !mainInfo.IsDir() {
		if mainGitDir = codexReadGitdirFile(mainDotGit); mainGitDir == "" {
			return ""
		}
	}
	if canonical, err := filepath.EvalSymlinks(mainGitDir); err != nil || canonical != commonDir {
		return ""
	}
	return mainRoot
}

func codexNearestGitAncestor(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

const codexMaxGitMetadataBytes = 64 * 1024

func codexGitMetadataFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() <= codexMaxGitMetadataBytes
}

func codexReadGitMetadata(path string) string {
	if !codexGitMetadataFile(path) {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > codexMaxGitMetadataBytes {
		return ""
	}
	return string(bytes.TrimSpace(raw))
}

// codexReadGitdirFile reads a "gitdir: <path>" pointer, relative to the
// pointer's directory.
func codexReadGitdirFile(path string) string {
	content := codexReadGitMetadata(path)
	target, ok := bytes.CutPrefix([]byte(content), []byte("gitdir:"))
	if !ok {
		return ""
	}
	target = bytes.TrimSpace(target)
	if len(target) == 0 {
		return ""
	}
	return codexJoinNative(filepath.Dir(path), string(target))
}

func codexJoinNative(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
