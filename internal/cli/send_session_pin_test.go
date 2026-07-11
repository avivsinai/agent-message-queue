package cli

import (
	"path/filepath"
	"testing"
)

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
	sourceRoot := sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice", "bob")

	t.Setenv("AM_ROOT", sourceRoot)
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
