//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCoopExecBootstrapsFreshGitWorktreeWithoutConsultingHomeAmqrc(t *testing.T) {
	repo, homeBase := enterFreshGitBootstrapProject(t, false)
	before := snapshotTreeDigest(t, homeBase)
	homeRead := false
	originalReadFile := globalAmqrcReadFile
	globalAmqrcReadFile = func(path string) ([]byte, error) {
		homeRead = true
		return originalReadFile(path)
	}
	t.Cleanup(func() { globalAmqrcReadFile = originalReadFile })

	execEnv := captureCoopExecEnvironment(t, []string{"--no-wake", "--me", "alice", "sh"})
	wantBase := filepath.Join(repo, defaultCoopRoot)
	wantRoot := filepath.Join(wantBase, defaultSessionName)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want repo-local bootstrap %q", got, wantRoot)
	}
	if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, wantBase) {
		t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, wantBase)
	}
	if got := envValue(execEnv, envSession); got != defaultSessionName {
		t.Fatalf("coop exec AM_SESSION = %q, want %q", got, defaultSessionName)
	}
	if _, err := os.Stat(filepath.Join(repo, ".amqrc")); err != nil {
		t.Fatalf("repo-local .amqrc missing after bootstrap: %v", err)
	}
	if after := snapshotTreeDigest(t, homeBase); after != before {
		t.Fatalf("bootstrap consulted or mutated HOME .amqrc target: before=%s after=%s", before, after)
	}
	if homeRead {
		t.Fatal("bootstrap consulted HOME .amqrc")
	}
}

func TestCoopExecBootstrapRequiresProvenGitWorktree(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "empty git directory",
			setup: func(t *testing.T, project string) {
				if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "arbitrary git file",
			setup: func(t *testing.T, project string) {
				if err := os.WriteFile(filepath.Join(project, ".git"), []byte("not a gitdir\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked git marker",
			setup: func(t *testing.T, project string) {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				runGitForTest(t, target, "init")
				if err := os.Symlink(filepath.Join(target, ".git"), filepath.Join(project, ".git")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			test.setup(t, project)
			enterBootstrapEnvironment(t, project)
			assertBootstrapRefusedWithoutWrites(t, project)
		})
	}

	t.Run("git marker inspection failure", func(t *testing.T) {
		project, _ := enterFreshGitBootstrapProject(t, false)
		marker := filepath.Join(project, ".git")
		original := gitMarkerLstat
		gitMarkerLstat = func(path string) (os.FileInfo, error) {
			if sameTreeIdentity(path, marker) {
				return nil, os.ErrPermission
			}
			return os.Lstat(path)
		}
		t.Cleanup(func() { gitMarkerLstat = original })
		assertBootstrapRefusedWithoutWrites(t, project)
	})
}

func assertBootstrapRefusedWithoutWrites(t *testing.T, project string) {
	t.Helper()
	err := runCoopExec([]string{"--no-wake", "--me", "alice", "sh"})
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("coop exec error = %v, want context-mismatch refusal", err)
	}
	for _, name := range []string{".amqrc", defaultCoopRoot, ".gitignore"} {
		if _, statErr := os.Lstat(filepath.Join(project, name)); !os.IsNotExist(statErr) {
			t.Fatalf("refused bootstrap created %s: %v", name, statErr)
		}
	}
}

func enterFreshGitBootstrapProject(t *testing.T, nested bool) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for repository bootstrap")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, repo, "init")
	cwd := repo
	if nested {
		cwd = filepath.Join(repo, "nested")
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	homeBase := enterBootstrapEnvironment(t, cwd)
	return repo, homeBase
}

func enterBootstrapEnvironment(t *testing.T, cwd string) string {
	t.Helper()
	clearCoopSessionPinForTest(t)
	for _, key := range []string{envRoot, envMe, envGlobalRoot} {
		setOptionalEnv(t, key, "", false)
	}
	fakeHome := t.TempDir()
	homeBase := filepath.Join(fakeHome, "other-project", defaultCoopRoot)
	if err := fsq.EnsureRootDirs(homeBase); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(`{"root":"`+homeBase+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Chdir(cwd)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)
	return homeBase
}
