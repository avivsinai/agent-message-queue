package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendRefusesPinnedRootWhenCwdHasRepoLocalSession(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global project")
	repoProject := filepath.Join(parent, "snagline project")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
	repoRoot := sessionRoot(t, repoProject, "session1", "alice", "bob")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(repoProject); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")

	err = runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong project"})
	assertConsumeRefused(t, err, "send")
	if !strings.Contains(err.Error(), "--root '") ||
		!strings.Contains(err.Error(), "snagline project/.agent-mail/session1' --me") {
		t.Fatalf("repin guidance does not shell-quote repo-local root: %v", err)
	}
	if got := inboxCount(t, globalRoot, "bob"); got != 0 {
		t.Fatalf("ambiguous send delivered %d message(s) to pinned global root", got)
	}
	if got := inboxCount(t, repoRoot, "bob"); got != 0 {
		t.Fatalf("ambiguous send delivered %d message(s) to repo-local root", got)
	}
}

func TestSendHonorsVerifiedSessionlessRootWhenCwdHasDifferentRepoQueue(t *testing.T) {
	project := t.TempDir()
	localRoot := filepath.Join(project, ".agent-mail")
	targetRoot := filepath.Join(localRoot, "squad", "v2-25-1")
	for _, root := range []string{localRoot, targetRoot} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("initialize %s/%s: %v", root, agent, err)
			}
		}
		configureSendTestRoot(t, root, "alice", "bob")
	}
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
	t.Chdir(project)
	pinSendSessionForTest(t, targetRoot, targetRoot, "")

	if err := runSend([]string{"--me", "alice", "--to", "bob", "--body", "verified root"}); err != nil {
		t.Fatalf("verified sessionless root should outrank cwd discovery: %v", err)
	}
	if got := inboxCount(t, targetRoot, "bob"); got != 1 {
		t.Fatalf("identity-pinned inbox count = %d, want 1", got)
	}
	if got := inboxCount(t, localRoot, "bob"); got != 0 {
		t.Fatalf("send touched cwd-local inbox: %d message(s)", got)
	}
}

func TestSendRefusesForeignCollabPinWhenCwdHasOnlyInitializedAuthSession(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "collab", "alice", "bob")
	localAuth := sessionRoot(t, repoProject, "auth", "alice", "bob")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(repoProject); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	pinSendSessionForTest(t, globalBase, globalRoot, "collab")

	err = runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong project"})
	assertConsumeRefused(t, err, "send")
	if got := inboxCount(t, globalRoot, "bob"); got != 0 {
		t.Fatalf("ambiguous send delivered %d message(s) to foreign collab", got)
	}
	if got := inboxCount(t, localAuth, "bob"); got != 0 {
		t.Fatalf("ambiguous send touched local auth inbox: %d message(s)", got)
	}
}

func TestSendHonorsUnpinnedAMRootWhenCwdHasInitializedLocalQueue(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalRoot := sessionRoot(t, globalProject, "collab", "alice", "bob")
	localRoot := sessionRoot(t, repoProject, "collab", "alice", "bob")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(repoProject); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	clearSendMailboxTestEnv(t)
	t.Setenv(envRoot, globalRoot)

	if err := runSend([]string{"--me", "alice", "--to", "bob", "--body", "deliberate AM_ROOT"}); err != nil {
		t.Fatalf("unpinned AM_ROOT send should honor the configured foreign root: %v", err)
	}
	if got := inboxCount(t, globalRoot, "bob"); got != 1 {
		t.Fatalf("foreign AM_ROOT inbox count = %d, want 1", got)
	}
	if got := inboxCount(t, localRoot, "bob"); got != 0 {
		t.Fatalf("unpinned AM_ROOT send touched local inbox: %d message(s)", got)
	}
}

func TestSendRefusesMismatchedPinnedSourceSession(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice", "bob")
	targetRoot := sessionRoot(t, parent, "session2", "alice", "bob")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong source"})
	assertConsumeRefused(t, err, "send")
	if got := inboxCount(t, targetRoot, "bob"); got != 0 {
		t.Fatalf("mismatched local send delivered %d message(s)", got)
	}
}

func TestSendTargetSessionDoesNotAuthorizeMismatchedSource(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	ambientRoot := sessionRoot(t, parent, "session2", "alice")
	targetRoot := sessionRoot(t, parent, "session3", "bob")

	t.Setenv("AM_ROOT", ambientRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runSend([]string{"--me", "alice", "--to", "bob", "--session", "session3", "--body", "wrong source"})
	assertConsumeRefused(t, err, "send")
	if got := inboxCount(t, targetRoot, "bob"); got != 0 {
		t.Fatalf("target routing authorized mismatched source and delivered %d message(s)", got)
	}
}

func TestSendAllowsExplicitRootWhenIgnoringSessionPin(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice", "bob")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runSend([]string{
		"--root", targetRoot,
		"--me", "alice",
		"--to", "bob",
		"--body", "deliberate source override",
		"--ignore-session-pin",
	})
	if err != nil {
		t.Fatalf("explicit root plus pin override should allow send: %v", err)
	}
	if got := inboxCount(t, targetRoot, "bob"); got != 1 {
		t.Fatalf("target inbox count = %d, want 1", got)
	}
}

func TestSendRejectsPinOverrideWithoutExplicitRoot(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := sessionRoot(t, parent, "session1", "alice", "bob")

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runSend([]string{
		"--me", "alice",
		"--to", "bob",
		"--body", "ambient override",
		"--ignore-session-pin",
	})
	if err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("ambient send override should be a usage error, got %v", err)
	}
	if got := inboxCount(t, root, "bob"); got != 0 {
		t.Fatalf("ambient override delivered %d message(s)", got)
	}
}
