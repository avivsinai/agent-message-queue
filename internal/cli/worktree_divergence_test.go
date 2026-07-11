package cli

import (
	"os"
	"os/exec"
	"path/filepath"
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

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
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
