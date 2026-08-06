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

func TestBareGitSignalRejectsNonGitSpoofs(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "arbitrary HEAD file and directories",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("fixture data\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"objects", "refs"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "all directories",
			setup: func(t *testing.T, dir string) {
				for _, name := range []string{"HEAD", "objects", "refs"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "dangling symlinks",
			setup: func(t *testing.T, dir string) {
				for _, name := range []string{"HEAD", "objects", "refs"} {
					if err := os.Symlink(filepath.Join(dir, "missing-"+name), filepath.Join(dir, name)); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			test.setup(t, dir)
			if bareGitRepositorySignal(dir) {
				t.Fatal("non-Git marker spoof was treated as a bare repository")
			}
		})
	}
}

func TestBareGitSignalAcceptsPackedRefsWithoutRefsDirectory(t *testing.T) {
	bare := t.TempDir()
	writeBareGitShape(t, bare, true)
	if !bareGitRepositorySignal(bare) {
		t.Fatal("packed-refs-only bare repository was not detected")
	}
}

func TestBareGitSignalAcceptsDetachedSHA1AndSHA256HEADs(t *testing.T) {
	for _, test := range []struct {
		name      string
		oidLength int
	}{
		{name: "sha1", oidLength: 40},
		{name: "sha256", oidLength: 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			bare := t.TempDir()
			writeBareGitShape(t, bare, false)
			oid := ""
			for len(oid) < test.oidLength {
				oid += "a"
			}
			if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte(oid+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if !bareGitRepositorySignal(bare) {
				t.Fatalf("%d-hex detached HEAD bare repository was not detected", test.oidLength)
			}
		})
	}
}

func TestBareGitSignalFailsClosedOnMarkerInspectionError(t *testing.T) {
	bare := t.TempDir()
	writeBareGitShape(t, bare, false)
	head := filepath.Join(bare, "HEAD")
	original := gitMarkerLstat
	gitMarkerLstat = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(head) {
			return nil, os.ErrPermission
		}
		return os.Lstat(path)
	}
	t.Cleanup(func() { gitMarkerLstat = original })

	if !bareGitRepositorySignal(bare) {
		t.Fatal("bare repository marker inspection failure opened the routing boundary")
	}
}
