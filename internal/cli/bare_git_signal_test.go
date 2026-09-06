package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBareGitShape(t *testing.T, dir string, packedOnly bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if packedOnly {
		if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitBoundaryPrefersEnclosingWorktreeOverGitInternalsAndFixtures(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBareGitShape(t, filepath.Join(repo, ".git"), false)
	fixture := filepath.Join(repo, "testdata", "fixture.git")
	writeBareGitShape(t, fixture, false)

	for _, cwd := range []string{
		filepath.Join(repo, ".git"),
		filepath.Join(repo, ".git", "hooks"),
		fixture,
	} {
		t.Run(filepath.Base(cwd), func(t *testing.T) {
			t.Chdir(cwd)
			top, ok := gitWorktreeRootFromCWD()
			if !ok {
				t.Fatal("Git worktree boundary was not detected")
			}
			expectSamePath(t, top, repo)
		})
	}
}
