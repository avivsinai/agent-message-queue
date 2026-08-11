package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestDoctorOpsHintsPerWorktreeSessionForLocalRootSources(t *testing.T) {
	tests := []struct {
		name       string
		rootSource string
		rcRoot     string
		wantHint   bool
	}{
		{name: "relative project config", rootSource: string(rootSourceProjectRC), rcRoot: ".agent-mail", wantHint: true},
		{name: "auto detected root", rootSource: string(rootSourceAutoDetect), rcRoot: ".agent-mail", wantHint: true},
		{name: "absolute project config", rootSource: string(rootSourceProjectRC), rcRoot: "absolute", wantHint: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, linked := createGitWorktreeFixture(t)
			t.Chdir(linked)

			root := filepath.Join(linked, ".agent-mail", "collab")
			ensureOpsRoot(t, root, "alice")
			if top, err := gitTopLevel(linked); err != nil || top != canonicalDiagnosticPath(linked) {
				t.Fatalf("gitTopLevel(%s)=%q, %v", linked, top, err)
			}
			if session := validSessionNameForRoot(root); session != "collab" {
				t.Fatalf("validSessionNameForRoot(%s)=%q, want collab", root, session)
			}
			rcRoot := tt.rcRoot
			if rcRoot == "absolute" {
				rcRoot = filepath.Join(t.TempDir(), "shared-agent-mail")
			}
			if err := os.WriteFile(filepath.Join(linked, ".amqrc"), []byte(`{"root":"`+rcRoot+`"}`), 0o600); err != nil {
				t.Fatalf("write linked .amqrc: %v", err)
			}

			result := runOpsChecks(root, tt.rootSource, false)
			hint, found := findOpsHint(result.Hints, "worktree_session_isolation")
			if found != tt.wantHint {
				t.Fatalf("worktree_session_isolation found=%v, want %v; hints=%#v", found, tt.wantHint, result.Hints)
			}
			if !found {
				return
			}
			for _, want := range []string{root, "collab", "per-worktree", "absolute"} {
				if !strings.Contains(hint.Message, want) {
					t.Fatalf("hint missing %q: %q", want, hint.Message)
				}
			}
		})
	}
}

func TestDoctorOpsWarnsWhenPeerPresenceIsFresherInSameSessionOtherWorktree(t *testing.T) {
	primary, linked := createGitWorktreeFixture(t)
	t.Chdir(primary)
	t.Setenv(envMe, "alice")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong-git-dir"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(t.TempDir(), "wrong-worktree"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(t.TempDir(), "wrong-common-dir"))

	currentRoot := filepath.Join(primary, ".agent-mail", "collab")
	otherRoot := filepath.Join(linked, ".agent-mail", "collab")
	for _, root := range []string{currentRoot, otherRoot} {
		ensureOpsRoot(t, root, "alice", "bob")
	}
	worktrees, err := listGitWorktrees(primary)
	if err != nil {
		t.Fatalf("listGitWorktrees: %v", err)
	}
	if len(worktrees) != 2 {
		t.Fatalf("worktrees=%#v, want primary and linked", worktrees)
	}
	now := time.Now()
	if err := presence.Write(currentRoot, presence.New("bob", "active", "current", now.Add(-time.Minute))); err != nil {
		t.Fatalf("write current presence: %v", err)
	}
	if err := presence.Write(otherRoot, presence.New("bob", "active", "other", now)); err != nil {
		t.Fatalf("write other presence: %v", err)
	}

	result := runOpsChecks(currentRoot, string(rootSourceProjectRC), false)
	hint, found := findOpsHint(result.Hints, "worktree_divergence")
	if !found {
		t.Fatalf("doctor ops missing worktree_divergence hint: %#v", result.Hints)
	}
	if hint.Status != "warn" {
		t.Fatalf("worktree divergence status=%q, want warn", hint.Status)
	}
	for _, want := range []string{canonicalDiagnosticPath(currentRoot), canonicalDiagnosticPath(otherRoot), "collab", "bob", ".amqrc", "AMQ_GLOBAL_ROOT"} {
		if !strings.Contains(hint.Message, want) {
			t.Fatalf("divergence hint missing %q: %q", want, hint.Message)
		}
	}
}

func TestDoctorOpsDoesNotWarnForOlderPeerOrFresherCallerPresence(t *testing.T) {
	primary, linked := createGitWorktreeFixture(t)
	t.Chdir(primary)
	t.Setenv(envMe, "alice")

	currentRoot := filepath.Join(primary, ".agent-mail", "collab")
	otherRoot := filepath.Join(linked, ".agent-mail", "collab")
	for _, root := range []string{currentRoot, otherRoot} {
		ensureOpsRoot(t, root, "alice", "bob")
	}
	now := time.Now()
	for _, item := range []struct {
		root   string
		agent  string
		seenAt time.Time
	}{
		{root: currentRoot, agent: "bob", seenAt: now},
		{root: otherRoot, agent: "bob", seenAt: now.Add(-time.Minute)},
		{root: currentRoot, agent: "alice", seenAt: now.Add(-time.Minute)},
		{root: otherRoot, agent: "alice", seenAt: now},
	} {
		if err := presence.Write(item.root, presence.New(item.agent, "active", "", item.seenAt)); err != nil {
			t.Fatalf("write %s presence at %s: %v", item.agent, item.root, err)
		}
	}

	result := runOpsChecks(currentRoot, string(rootSourceProjectRC), false)
	if hint, found := findOpsHint(result.Hints, "worktree_divergence"); found {
		t.Fatalf("unexpected divergence hint: %#v", hint)
	}
}

func TestWorktreeDiagnosticsFailOpenOutsideGitRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	if hints := checkLinkedWorktreeLocalHint(filepath.Join(t.TempDir(), ".agent-mail", "collab"), string(rootSourceAutoDetect)); len(hints) != 0 {
		t.Fatalf("local hints outside git=%#v, want none", hints)
	}
	if hints := checkWorktreeDivergenceHints(filepath.Join(t.TempDir(), ".agent-mail", "collab"), []string{"alice"}); len(hints) != 0 {
		t.Fatalf("divergence hints outside git=%#v, want none", hints)
	}
}

func createGitWorktreeFixture(t *testing.T) (primary, linked string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for worktree diagnostics")
	}
	parent := filepath.Join(t.TempDir(), "worktree parent")
	primary = filepath.Join(parent, "primary")
	linked = filepath.Join(parent, "linked")
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(primary, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitForTest(t, primary, "init")
	runGitForTest(t, primary, "add", ".amqrc", "README.md")
	runGitForTest(t, primary, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "fixture")
	runGitForTest(t, primary, "worktree", "add", "-b", "linked", linked)
	return primary, linked
}

// gitRepoSelectionEnv lists the variables through which an enclosing git
// process — a pre-push hook running `make ci`, most commonly — redirects git
// away from the `-C` target. A leaked GIT_DIR pointed every fixture command
// at the enclosing repository itself, committing fixture content onto real
// branches and re-initializing the real repo as bare. Mirrors the unset list
// in scripts/smoke-test.sh. The contract here is direct repo-selection
// isolation only; scrubbing git's full local environment (config injection,
// grafts, shallow files) is the generated pre-push hook's job.
var gitRepoSelectionEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_COMMON_DIR", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

func gitEnvForTest() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		// GIT_CONFIG_NOSYSTEM is filtered too: git honors the first
		// occurrence, so an inherited empty value would defeat the "=1"
		// appended below.
		if slices.Contains(gitRepoSelectionEnv, name) || name == "GIT_CONFIG_NOSYSTEM" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1")
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvForTest()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutputForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvForTest()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// TestRunGitForTestIsolatesEnclosingRepo executes the incident that motivated
// gitEnvForTest: a pre-push hook leaked GIT_DIR into `make ci`, and every
// fixture git command then mutated the enclosing repository — a fixture
// commit landed on a real branch and `init --bare` flipped core.bare on the
// real repo. Before the scrub, this test fails with the victim corrupted.
func TestRunGitForTestIsolatesEnclosingRepo(t *testing.T) {
	victim := t.TempDir()
	runGitForTest(t, victim, "init")
	runGitForTest(t, victim, "-c", "user.name=Victim", "-c", "user.email=victim@example.invalid",
		"commit", "--allow-empty", "-m", "base")
	victimHead := gitOutputForTest(t, victim, "rev-parse", "HEAD")

	t.Setenv("GIT_DIR", filepath.Join(victim, ".git"))
	t.Setenv("GIT_WORK_TREE", victim)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(victim, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(victim, ".git", "objects"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(victim, ".git"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")

	nosystem := 0
	for _, entry := range gitEnvForTest() {
		if strings.HasPrefix(entry, "GIT_CONFIG_NOSYSTEM=") {
			nosystem++
			if entry != "GIT_CONFIG_NOSYSTEM=1" {
				t.Fatalf("gitEnvForTest emitted %q, want GIT_CONFIG_NOSYSTEM=1", entry)
			}
		}
	}
	if nosystem != 1 {
		t.Fatalf("gitEnvForTest emitted GIT_CONFIG_NOSYSTEM %d times, want exactly once", nosystem)
	}

	// The incident sequence: fixture init, commit, worktree add, and a bare
	// init, all under the hostile environment above.
	fixture := filepath.Join(t.TempDir(), "primary")
	if err := os.Mkdir(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, fixture, "init")
	runGitForTest(t, fixture, "add", "README.md")
	runGitForTest(t, fixture, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid",
		"commit", "-m", "fixture")
	runGitForTest(t, fixture, "worktree", "add", "-b", "linked", filepath.Join(t.TempDir(), "linked"))
	bare := filepath.Join(t.TempDir(), "bare")
	if err := os.Mkdir(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, bare, "init", "--bare")

	if head := gitOutputForTest(t, victim, "rev-parse", "HEAD"); head != victimHead {
		t.Fatalf("victim HEAD moved from %s to %s", victimHead, head)
	}
	if count := gitOutputForTest(t, victim, "rev-list", "--count", "HEAD"); count != "1" {
		t.Fatalf("victim commit count = %s, want 1", count)
	}
	if bareFlag := gitOutputForTest(t, victim, "config", "core.bare"); bareFlag != "false" {
		t.Fatalf("victim core.bare = %s, want false", bareFlag)
	}
	if subject := gitOutputForTest(t, victim, "log", "-1", "--format=%s"); subject != "base" {
		t.Fatalf("victim HEAD subject = %q, want %q", subject, "base")
	}
}

func ensureOpsRoot(t *testing.T, root string, agents ...string) {
	t.Helper()
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s, %s): %v", root, agent, err)
		}
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  agents,
	}, true); err != nil {
		t.Fatalf("write config at %s: %v", root, err)
	}
}

func findOpsHint(hints []opsHint, code string) (opsHint, bool) {
	for _, hint := range hints {
		if hint.Code == code {
			return hint, true
		}
	}
	return opsHint{}, false
}
