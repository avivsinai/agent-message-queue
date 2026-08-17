//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
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

func TestCoopExecNamedSessionBootstrapsFreshGitWorktree(t *testing.T) {
	repo, _ := enterFreshGitBootstrapProject(t, false)

	execEnv := captureCoopExecEnvironment(t, []string{"--session", "session1", "--no-wake", "--me", "alice", "sh"})
	wantBase := filepath.Join(repo, defaultCoopRoot)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, filepath.Join(wantBase, "session1")) {
		t.Fatalf("coop exec AM_ROOT = %q, want named bootstrap under %q", got, wantBase)
	}
	if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, wantBase) {
		t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, wantBase)
	}
}

func TestCoopExecBootstrapFromSubdirectoryUsesGitTop(t *testing.T) {
	repo, _ := enterFreshGitBootstrapProject(t, true)

	execEnv := captureCoopExecEnvironment(t, []string{"--no-wake", "--me", "alice", "sh"})
	wantBase := filepath.Join(repo, defaultCoopRoot)
	if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, wantBase) {
		t.Fatalf("coop exec AM_BASE_ROOT = %q, want Git top %q", got, wantBase)
	}
	if _, err := os.Stat(filepath.Join(repo, "nested", defaultCoopRoot)); !os.IsNotExist(err) {
		t.Fatalf("bootstrap scattered queue below Git top: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "nested", ".amqrc")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap scattered .amqrc below Git top: %v", err)
	}
}

func TestCoopInitBootstrapFromSubdirectoryUsesGitTop(t *testing.T) {
	repo, _ := enterFreshGitBootstrapProject(t, true)

	if err := runCoopInitInternal([]string{"--no-gitignore", "--json"}, false); err != nil {
		t.Fatalf("coop init from subdirectory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".amqrc")); err != nil {
		t.Fatalf("Git-top .amqrc missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, defaultCoopRoot)); err != nil {
		t.Fatalf("Git-top queue missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "nested", defaultCoopRoot)); !os.IsNotExist(err) {
		t.Fatalf("coop init scattered queue below Git top: %v", err)
	}
}

func TestCoopExecBootstrapsLinkedWorktreeLocallyAndDoctorDetectsDivergence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for linked-worktree bootstrap")
	}
	parent := t.TempDir()
	primary := filepath.Join(parent, "primary")
	linked := filepath.Join(parent, "linked")
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, primary, "init")
	runGitForTest(t, primary, "add", "README.md")
	runGitForTest(t, primary, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "fixture")
	runGitForTest(t, primary, "worktree", "add", "-b", "linked-bootstrap", linked)
	enterBootstrapEnvironment(t, linked)

	execEnv := captureCoopExecEnvironment(t, []string{"--no-wake", "--me", "alice", "sh"})
	linkedRoot := filepath.Join(linked, defaultCoopRoot, defaultSessionName)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, linkedRoot) {
		t.Fatalf("linked-worktree AM_ROOT = %q, want %q", got, linkedRoot)
	}
	if _, err := os.Stat(filepath.Join(primary, defaultCoopRoot)); !os.IsNotExist(err) {
		t.Fatalf("linked bootstrap mutated primary worktree: %v", err)
	}

	primaryRoot := filepath.Join(primary, defaultCoopRoot, defaultSessionName)
	for _, root := range []string{primaryRoot, linkedRoot} {
		ensureOpsRoot(t, root, "alice", "bob")
	}
	now := time.Now()
	if err := presence.Write(linkedRoot, presence.New("bob", "active", "linked", now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := presence.Write(primaryRoot, presence.New("bob", "active", "primary", now)); err != nil {
		t.Fatal(err)
	}
	result := runOpsChecks(linkedRoot, string(rootSourceProjectRC), false)
	if _, found := findOpsHint(result.Hints, "worktree_divergence"); !found {
		t.Fatalf("doctor --ops did not report cross-worktree divergence: %#v", result.Hints)
	}
}

func TestFreshGitParticipatingCommandsKeepRefusingWithBootstrapRemedy(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "send", run: func() error { return runSend([]string{"--me", "alice", "--to", "bob", "--body", "no route"}) }},
		{name: "list", run: func() error { return runList([]string{"--me", "alice", "--new"}) }},
		{name: "drain", run: func() error { return runDrain([]string{"--me", "alice"}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enterFreshGitBootstrapProject(t, false)
			err := test.run()
			if err == nil || GetExitCode(err) != ExitContextMismatch {
				t.Fatalf("%s error = %v, want context-mismatch refusal", test.name, err)
			}
			if !strings.Contains(err.Error(), "amq coop init") {
				t.Fatalf("%s refusal missing runnable bootstrap remedy: %v", test.name, err)
			}
		})
	}
}

func TestCoopExecNoInitKeepsFreshGitRefusal(t *testing.T) {
	for _, args := range [][]string{
		{"--no-init", "--no-wake", "--me", "alice", "sh"},
		{"--session", "session1", "--no-init", "--no-wake", "--me", "alice", "sh"},
	} {
		repo, _ := enterFreshGitBootstrapProject(t, false)
		err := runCoopExec(args)
		if err == nil || GetExitCode(err) != ExitContextMismatch {
			t.Fatalf("coop exec %v error = %v, want context-mismatch refusal", args, err)
		}
		if !strings.Contains(err.Error(), "amq coop init") {
			t.Fatalf("coop exec %v refusal missing runnable remedy: %v", args, err)
		}
		if _, statErr := os.Stat(filepath.Join(repo, defaultCoopRoot)); !os.IsNotExist(statErr) {
			t.Fatalf("coop exec %v mutated fresh repo: %v", args, statErr)
		}
	}
}

func TestCoopExecRefusesBareRepositoryBootstrap(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for bare-repository bootstrap")
	}
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, bare, "init", "--bare")
	enterBootstrapEnvironment(t, bare)

	for _, args := range [][]string{
		{"--no-wake", "--me", "alice", "sh"},
		{"--session", "session1", "--no-wake", "--me", "alice", "sh"},
	} {
		err := runCoopExec(args)
		if err == nil || GetExitCode(err) != ExitContextMismatch {
			t.Fatalf("bare-repository coop exec %v error = %v, want context-mismatch refusal", args, err)
		}
		for _, want := range []string{"cd into a worktree", "--root"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("bare-repository refusal missing %q: %v", want, err)
			}
		}
	}
	if _, statErr := os.Stat(filepath.Join(bare, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("bare-repository bootstrap created a queue: %v", statErr)
	}
	if err := runCoopInitInternal([]string{"--no-gitignore"}, false); err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("bare-repository coop init error = %v, want context-mismatch refusal", err)
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
