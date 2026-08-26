package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// seedParentProject runs a complete setup in dir so it owns a committed
// .amqrc, .amq/launch.json, and a provisioned base root, then turns it into a
// Git repo. A nested non-Git directory under it resolves the parent top
// through gitWorktreeTopFromCWD.
func seedParentProject(t *testing.T, dir string) {
	t.Helper()
	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--json"})
	}); err != nil {
		t.Fatalf("seed parent setup: %v", err)
	}
	runGitForTest(t, dir, "init")
	runGitForTest(t, dir, "add", ".amqrc", setupConfigPath)
	runGitForTest(t, dir, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "fixture")
}

// TestSetupWithoutProjectRootFlagResolvesParentWorktreeTop covers the unchanged
// v0.61 behavior (issue #648 item 1(a)): a nested scratch directory under a
// parent Git repo, without --project-root and without a local config, resolves
// the Git worktree top (the parent) as the project root. The flag is the
// opt-in override.
func TestSetupWithoutProjectRootFlagResolvesParentWorktreeTop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for nested project-root authority tests")
	}
	parent := setupProjectFixture(t, "claude")
	seedParentProject(t, parent)
	parentMail := filepath.Join(parent, defaultCoopRoot)

	nested := filepath.Join(parent, "nested", "scratch")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()

	_, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--json"})
	})
	if err != nil {
		t.Fatalf("nested setup without flag: %v", err)
	}
	// The parent base root gained a collab session: project root resolved to
	// the parent top, so writes landed there, not in the nested dir.
	if _, statErr := os.Stat(filepath.Join(parentMail, "collab")); statErr != nil {
		t.Fatalf("unchanged-behavior run did not provision parent collab session: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(nested, ".amqrc")); !os.IsNotExist(statErr) {
		t.Fatalf("unchanged-behavior run wrote nested .amqrc: %v", statErr)
	}
}

// TestSetupProjectRootFlagRefusesParentAmqrcAndLeavesParentUntouched covers the
// refusal path (issue #648 item 1(a)): when --project-root names a nested dir
// but a parent .amqrc exists, setup refuses adoption and names the parent
// path (explicit beats inferred). The parent's .amqrc, .amq/launch.json, and
// .agent-mail are left byte-for-byte unchanged.
func TestSetupProjectRootFlagIgnoresParentAmqrcAndWritesNestedConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for nested project-root authority tests")
	}
	parent := setupProjectFixture(t, "claude")
	seedParentProject(t, parent)

	parentAmqrc := filepath.Join(parent, ".amqrc")
	parentAmqrcBefore, err := os.ReadFile(parentAmqrc)
	if err != nil {
		t.Fatal(err)
	}
	parentConfig := filepath.Join(parent, setupConfigPath)
	parentConfigBefore, err := os.ReadFile(parentConfig)
	if err != nil {
		t.Fatal(err)
	}
	parentMail := filepath.Join(parent, defaultCoopRoot)
	parentMailBefore := setupTreeDigest(t, parentMail)

	nested := filepath.Join(parent, "nested", "scratch")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()

	_, err = captureEnvStdout(t, func() error {
		return runSetup([]string{
			"-y", "--agents", "claude", "--default-session", "collab",
			"--launcher-preference", "commands", "--project-root", nested, "--json",
		})
	})
	if err != nil {
		t.Fatalf("setup with --project-root and parent .amqrc: %v", err)
	}
	// The explicit project root owns its config: .amqrc and .amq/launch.json
	// land in the nested dir, not the parent.
	if _, statErr := os.Stat(filepath.Join(nested, ".amqrc")); statErr != nil {
		t.Fatalf("--project-root did not write nested .amqrc: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(nested, setupConfigPath)); statErr != nil {
		t.Fatalf("--project-root did not write nested project config: %v", statErr)
	}
	// The parent's .amqrc and project config are byte-for-byte unchanged.
	if after, readErr := os.ReadFile(parentAmqrc); readErr != nil || string(after) != string(parentAmqrcBefore) {
		t.Fatalf("parent .amqrc changed: before=%q after=%q err=%v", parentAmqrcBefore, after, readErr)
	}
	if after, readErr := os.ReadFile(parentConfig); readErr != nil || string(after) != string(parentConfigBefore) {
		t.Fatalf("parent project config changed: before=%q after=%q err=%v", parentConfigBefore, after, readErr)
	}
	// The parent's live base root is byte-for-byte unchanged.
	if after := setupTreeDigest(t, parentMail); after != parentMailBefore {
		t.Fatalf("parent base root changed during nested setup")
	}
	// Discovery is suppressed: a subsequent findAndLoadAmqrc from the nested
	// dir resolves the NESTED .amqrc, never the parent's. This is the
	// assertion that proves upward base discovery no longer redirects writes
	// into a parent repo's live base root.
	resetAmqrcCache()
	result, findErr := findAndLoadAmqrc()
	if findErr != nil {
		t.Fatalf("findAndLoadAmqrc from nested: %v", findErr)
	}
	if !sameTreeIdentity(result.Dir, nested) {
		t.Fatalf("findAndLoadAmqrc resolved %s, want nested %s", result.Dir, nested)
	}
}

// TestSetupProjectRootFlagOwnsNestedDirWithoutParentAmqrc covers the write path
// (issue #648 item 1(a)): with --project-root naming a nested dir that has no
// parent .amqrc, setup treats that dir as the project root and writes .amqrc,
// .amq/launch.json, and .gitignore there. No chdir to a Git worktree top
// redirects the writes.
func TestSetupProjectRootFlagOwnsNestedDirWithoutParentAmqrc(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for nested project-root authority tests")
	}
	parent := setupProjectFixture(t, "claude")
	// A parent Git repo with NO .amqrc: the nested dir is the sole project
	// root authority through the flag.
	runGitForTest(t, parent, "init")
	runGitForTest(t, parent, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "--allow-empty", "-m", "fixture")

	nested := filepath.Join(parent, "nested", "scratch")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()

	_, err := captureEnvStdout(t, func() error {
		return runSetup([]string{
			"-y", "--agents", "claude", "--default-session", "collab",
			"--launcher-preference", "commands", "--project-root", nested, "--json",
		})
	})
	if err != nil {
		t.Fatalf("nested setup with --project-root: %v", err)
	}
	// The flag owns its own project root: .amqrc and .amq/launch.json land in
	// the nested dir, not the parent.
	if _, statErr := os.Stat(filepath.Join(nested, ".amqrc")); statErr != nil {
		t.Fatalf("--project-root did not write nested .amqrc: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(nested, setupConfigPath)); statErr != nil {
		t.Fatalf("--project-root did not write nested project config: %v", statErr)
	}
	// The parent gained no AMQ files.
	if _, statErr := os.Stat(filepath.Join(parent, ".amqrc")); !os.IsNotExist(statErr) {
		t.Fatalf("--project-root wrote parent .amqrc: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(parent, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("--project-root provisioned parent base root: %v", statErr)
	}
}

// TestSetupTreatsCwdWithLocalConfigAsProjectRootWithoutFlag covers issue #648
// item 1(a): an existing explicit .amq/launch.json in cwd is authority, so
// setup treats cwd as the project root without --project-root and does not
// redirect writes to a Git worktree top. The parent here owns no .amqrc, so
// there is no parent adoption to refuse; the local config alone pins the root.
func TestSetupTreatsCwdWithLocalConfigAsProjectRootWithoutFlag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for nested project-root authority tests")
	}
	parent := setupProjectFixture(t, "claude")
	// A parent Git repo with NO .amqrc: only the nested dir's local config
	// can pin the project root.
	runGitForTest(t, parent, "init")
	runGitForTest(t, parent, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	parentMail := filepath.Join(parent, defaultCoopRoot)

	// A nested dir that already owns .amq/launch.json is its own project root.
	nested := filepath.Join(parent, "nested", "owned")
	if err := os.MkdirAll(filepath.Join(nested, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: "collab", Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: launch.ResumeEnabled}},
	}
	projectData, err := launch.MarshalProjectConfig(projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, setupConfigPath), projectData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()

	_, err = captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--json"})
	})
	if err != nil {
		t.Fatalf("nested setup with local config: %v", err)
	}
	// The nested dir owns its base root; the parent never gains one.
	if _, statErr := os.Stat(filepath.Join(nested, ".amqrc")); statErr != nil {
		t.Fatalf("local-config cwd did not write nested .amqrc: %v", statErr)
	}
	if _, statErr := os.Stat(parentMail); !os.IsNotExist(statErr) {
		t.Fatalf("parent base root was created when cwd owned a local config: %v", statErr)
	}
}

// TestSetupProjectRootFlagValidation covers the flag's own input validation.
func TestSetupProjectRootFlagValidation(t *testing.T) {
	setupProjectFixture(t, "claude")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "blank project root", args: []string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--project-root", "  "}, want: "must not be blank"},
		{name: "missing project root", args: []string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--project-root", "/definitely/does/not/exist/xyz"}, want: "does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runSetup(test.args)
			if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v (exit=%d), want usage containing %q", err, GetExitCode(err), test.want)
			}
		})
	}
}
