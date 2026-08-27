package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// leftover after #663: a nested/linked worktree under a parent that already
// owns .amqrc must not silently adopt the parent live base root. #663 covered
// amq setup --project-root (and cwd-owned .amq/launch.json). Omri on v0.73.0
// still reported the same discovery seam for a nested checkout that is itself
// a worktree, not a scratch subdirectory.

func TestNestedWorktreeDoesNotAdoptParentLiveBaseRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for nested worktree discovery tests")
	}

	t.Run("nested independent checkout without local config fails closed", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedIndependentWorktree(t)
		enterIsolatedDiscoveryCwd(t, nested)
		assertDoesNotAdoptParentLiveRoot(t, parent, parentMail, nested)
	})

	t.Run("nested linked worktree without local config fails closed", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedLinkedWorktree(t, false)
		enterIsolatedDiscoveryCwd(t, nested)
		assertDoesNotAdoptParentLiveRoot(t, parent, parentMail, nested)
	})

	t.Run("nested linked worktree with own relative .amqrc stays local", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedLinkedWorktree(t, true)
		enterIsolatedDiscoveryCwd(t, nested)

		result, err := findAndLoadAmqrc()
		if err != nil {
			t.Fatalf("findAndLoadAmqrc: %v", err)
		}
		if !sameTreeIdentity(result.Dir, nested) {
			t.Fatalf("findAndLoadAmqrc resolved %s, want nested worktree %s", result.Dir, nested)
		}

		root, source, _, err := resolveEnvConfigWithSource("", "")
		if err != nil {
			t.Fatalf("resolveEnvConfigWithSource: %v", err)
		}
		want := filepath.Join(nested, defaultCoopRoot)
		expectSamePath(t, root, want)
		if source != rootSourceProjectRC {
			t.Fatalf("source = %q, want %q", source, rootSourceProjectRC)
		}
		if sameTreeIdentity(root, parentMail) {
			t.Fatalf("adopted parent live base root %s", parentMail)
		}

		discovered, found, err := resolveDiscoveredBaseRoot()
		if err != nil || !found {
			t.Fatalf("resolveDiscoveredBaseRoot found=%v err=%v", found, err)
		}
		expectSamePath(t, discovered, want)

		stdout, _, envErr := captureEnvOutput(t, func() error {
			return runEnv([]string{"--me", "claude", "--json"})
		})
		if envErr != nil {
			t.Fatalf("amq env: %v", envErr)
		}
		var out envOutput
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("env json: %v\n%s", err, stdout)
		}
		expectSamePath(t, out.Root, want)
		if sameTreeIdentity(out.BaseRoot, parentMail) || sameTreeIdentity(out.Root, parentMail) {
			t.Fatalf("amq env adopted parent live root: %+v", out)
		}

		assertParentMailUnchanged(t, parent, parentMail)
	})

	t.Run("nested checkout with cwd-owned launch.json does not adopt parent", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedIndependentWorktree(t)
		if err := os.MkdirAll(filepath.Join(nested, ".amq"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, setupConfigPath), []byte(`{"schema":1,"default_session":"collab","agents":[{"handle":"claude","adapter":"claude","command":["claude"]}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		enterIsolatedDiscoveryCwd(t, nested)
		assertDoesNotAdoptParentLiveRoot(t, parent, parentMail, nested)
	})

	t.Run("symlinked path into nested worktree does not adopt parent", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedIndependentWorktree(t)
		if err := os.MkdirAll(filepath.Join(nested, defaultCoopRoot, "agents"), 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(parent, "alias")
		if err := os.Symlink(nested, alias); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		aliasAbs, err := filepath.Abs(alias)
		if err != nil {
			t.Fatal(err)
		}
		enterIsolatedDiscoveryCwd(t, alias)
		t.Setenv("PWD", aliasAbs)

		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if sameCleanPath(cwd, nested) {
			t.Fatalf("os.Getwd resolved the alias to %s; need logical $PWD to exercise the ceiling comparison", cwd)
		}

		if got := resolveRoot(defaultCoopRoot); sameTreeIdentity(got, parentMail) {
			t.Fatalf("resolveRoot(%q) from symlinked cwd adopted parent live queue %s", defaultCoopRoot, parentMail)
		}
		if found, ok := findRootInParents(cwd, defaultCoopRoot); ok && sameTreeIdentity(found, parentMail) {
			t.Fatalf("findRootInParents via symlinked path escaped the ceiling: %s", found)
		}
		root, _, _, err := resolveEnvConfigWithSource("", "")
		if err != nil {
			t.Fatalf("resolveEnvConfigWithSource from alias: %v", err)
		}
		expectSamePath(t, root, filepath.Join(nested, defaultCoopRoot))
		if sameTreeIdentity(root, parentMail) {
			t.Fatalf("adopted parent live base root %s via symlink", parentMail)
		}
		if result, findErr := findAmqrcForRoot(filepath.Join(cwd, defaultCoopRoot)); findErr == nil {
			t.Fatalf("findAmqrcForRoot via symlinked path adopted %s", result.Path)
		} else if !errors.Is(findErr, errAmqrcNotFound) {
			t.Fatalf("findAmqrcForRoot via symlink = %v, want errAmqrcNotFound", findErr)
		}
		assertParentMailUnchanged(t, parent, parentMail)
	})

	t.Run("relative .agent-mail does not walk into parent live queue", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedIndependentWorktree(t)
		enterIsolatedDiscoveryCwd(t, nested)

		if got := resolveRoot(defaultCoopRoot); sameTreeIdentity(got, parentMail) {
			t.Fatalf("resolveRoot(%q) adopted parent live queue %s", defaultCoopRoot, parentMail)
		}
		if found, ok := findRootInParents(nested, defaultCoopRoot); ok {
			t.Fatalf("findRootInParents crossed nested worktree ceiling to %s", found)
		}

		t.Setenv(envRoot, defaultCoopRoot)
		root, _, _, err := resolveEnvConfigWithSource("", "")
		if err != nil {
			t.Fatalf("AM_ROOT=%s: %v", defaultCoopRoot, err)
		}
		expectSamePath(t, root, filepath.Join(nested, defaultCoopRoot))
		if sameTreeIdentity(root, parentMail) {
			t.Fatalf("relative AM_ROOT adopted parent live base root %s", parentMail)
		}

		if _, err := findAmqrcForRoot(filepath.Join(nested, defaultCoopRoot)); !errors.Is(err, errAmqrcNotFound) {
			t.Fatalf("findAmqrcForRoot from nested queue path = %v, want errAmqrcNotFound", err)
		}
		assertParentMailUnchanged(t, parent, parentMail)
	})

	t.Run("setup without --project-root stays in nested worktree", func(t *testing.T) {
		parent, parentMail, nested := seedParentWithNestedIndependentWorktree(t)
		parentAmqrcBefore, err := os.ReadFile(filepath.Join(parent, ".amqrc"))
		if err != nil {
			t.Fatal(err)
		}
		parentMailBefore := setupTreeDigest(t, parentMail)
		enterIsolatedDiscoveryCwd(t, nested)
		installSetupHarness(t)

		_, err = captureEnvStdout(t, func() error {
			return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--json"})
		})
		if err != nil {
			t.Fatalf("setup from nested worktree: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(nested, ".amqrc")); statErr != nil {
			t.Fatalf("nested setup did not write local .amqrc: %v", statErr)
		}
		if after, readErr := os.ReadFile(filepath.Join(parent, ".amqrc")); readErr != nil || string(after) != string(parentAmqrcBefore) {
			t.Fatalf("parent .amqrc changed: before=%q after=%q err=%v", parentAmqrcBefore, after, readErr)
		}
		if after := setupTreeDigest(t, parentMail); after != parentMailBefore {
			t.Fatalf("parent live base root mutated during nested setup")
		}
	})
}

func seedParentWithLiveQueue(t *testing.T) (parent, parentMail string) {
	t.Helper()
	workspace := t.TempDir()
	parent = filepath.Join(workspace, "parent")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	parentMail = filepath.Join(parent, defaultCoopRoot)
	if err := os.MkdirAll(filepath.Join(parentMail, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, ".amqrc"), []byte(`{"root":".agent-mail","project":"parent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "README.md"), []byte("parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, parent, "init")
	runGitForTest(t, parent, "add", "README.md")
	runGitForTest(t, parent, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "parent")
	return parent, parentMail
}

func seedParentWithNestedIndependentWorktree(t *testing.T) (parent, parentMail, nested string) {
	t.Helper()
	parent, parentMail = seedParentWithLiveQueue(t)
	nested = filepath.Join(parent, "nested", "checkout")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "README.md"), []byte("nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, nested, "init")
	runGitForTest(t, nested, "add", "README.md")
	runGitForTest(t, nested, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "nested")
	return parent, parentMail, nested
}

func seedParentWithNestedLinkedWorktree(t *testing.T, commitAmqrc bool) (parent, parentMail, nested string) {
	t.Helper()
	parent, parentMail = seedParentWithLiveQueue(t)
	if commitAmqrc {
		runGitForTest(t, parent, "add", ".amqrc")
		runGitForTest(t, parent, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "amqrc")
	}
	nested = filepath.Join(parent, "nested", "linked")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, parent, "worktree", "add", "-b", "linked", nested)
	return parent, parentMail, nested
}

func enterIsolatedDiscoveryCwd(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe, envGlobalRoot} {
		setOptionalEnv(t, key, "", false)
	}
	t.Chdir(dir)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)
}

func assertDoesNotAdoptParentLiveRoot(t *testing.T, parent, parentMail, nested string) {
	t.Helper()
	parentMailBefore := setupTreeDigest(t, parentMail)

	if _, err := findAndLoadAmqrc(); err == nil || !errors.Is(err, errAmqrcNotFound) {
		if err == nil {
			t.Fatal("findAndLoadAmqrc adopted a parent .amqrc from a nested worktree")
		}
		t.Fatalf("findAndLoadAmqrc = %v, want errAmqrcNotFound", err)
	}
	if got := detectAgentMailDir(); got != "" {
		t.Fatalf("detectAgentMailDir = %q, want empty", got)
	}

	_, _, _, err := resolveEnvConfigWithSource("", "")
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("resolveEnvConfigWithSource err = %v, want context mismatch", err)
	}
	if !strings.Contains(err.Error(), nested) {
		t.Fatalf("refusal must name the nested worktree %s: %v", nested, err)
	}
	if strings.Contains(err.Error(), parentMail) {
		t.Fatalf("refusal leaked parent live root %s: %v", parentMail, err)
	}

	if _, found, err := resolveDiscoveredBaseRoot(); err == nil || found || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("resolveDiscoveredBaseRoot found=%v err=%v, want context mismatch", found, err)
	}
	if _, err := resolveDefaultRoot(); err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("resolveDefaultRoot err = %v, want context mismatch", err)
	}

	stdout, _, envErr := captureEnvOutput(t, func() error {
		return runEnv([]string{"--me", "claude", "--json"})
	})
	if envErr == nil || GetExitCode(envErr) != ExitContextMismatch || stdout != "" {
		t.Fatalf("amq env stdout=%q err=%v, want empty stdout and exit 5", stdout, envErr)
	}

	if after := setupTreeDigest(t, parentMail); after != parentMailBefore {
		t.Fatalf("parent live base root mutated during nested discovery")
	}
	assertParentMailUnchanged(t, parent, parentMail)
}

func assertParentMailUnchanged(t *testing.T, parent, parentMail string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(parent, ".amqrc")); err != nil {
		t.Fatalf("parent .amqrc missing: %v", err)
	}
	if _, err := os.Stat(parentMail); err != nil {
		t.Fatalf("parent live queue missing: %v", err)
	}
}

func installSetupHarness(t *testing.T) {
	t.Helper()
	setupHarnessAdapters = func() []launch.HarnessAdapter {
		return []launch.HarnessAdapter{setupFixtureAdapter{name: "claude"}}
	}
	setupLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	setupCmuxAvailable = func() bool { return false }
	setupGhosttyAvailable = func() bool { return false }
	setupCommitStepHook = nil
	t.Cleanup(func() {
		setupHarnessAdapters = func() []launch.HarnessAdapter {
			return []launch.HarnessAdapter{
				launch.NewClaudeAdapter(launch.ClaudeProvider),
				launch.NewCodexAdapter(launch.CodexProvider),
				launch.NewCursorAdapter(setupCursorCommand()),
				launch.NewGrokAdapter(launch.GrokProvider),
			}
		}
		setupLookPath = execLookPathForSetup
		setupCmuxAvailable = setupCmuxAvailableDefault
		setupGhosttyAvailable = setupGhosttyAvailableDefault
		setupCommitStepHook = nil
	})
}
