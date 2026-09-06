package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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
