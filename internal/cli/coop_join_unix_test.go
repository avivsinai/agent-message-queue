//go:build darwin || linux

package cli

// Purpose: guards amq coop join and the identity a joined Claude Code
// session adopts. The first test breaks if a joined session does not get
// its handle and session root with no AM_* variable, if peers cannot send
// to a handle that only join created, if two running sessions can hold one
// handle, if a join in a session that has AM_* variables succeeds, or if a
// session that left keeps the identity. The second test is the regression
// test for a defect seen live in the desktop app: /clear ends the session's
// Claude Code process, the next prompt starts a session with a new id, and
// that session could not join with its own handle because the ended session
// held it and nothing could run amq coop leave there.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeSessionRecord writes the session record a Claude Code process pid
// keeps while it runs claudeSession, and returns its path. With the pid of
// this test's parent process, the test runs "inside" that session; with
// the pid of the test itself, the session runs somewhere else.
func claudeSessionRecord(t *testing.T, pid int, claudeSession string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	record := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"pidDomain":%q}`, pid, claudeSession, runtime.GOOS)
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoopJoinBindsTheSessionAndCommandsAdoptIt(t *testing.T) {
	clearSendMailboxTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	if _, err := captureStdout(t, func() error { return runCoopInit([]string{"--agents", "claude,codex", "--no-gitignore"}) }); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(project, defaultCoopRoot, defaultSessionName)
	const first, second = "7c29e148-77eb-47c0-8984-10c13b4700b1", "7c29e148-77eb-47c0-8984-10c13b4700b2"
	join := func(claudeSession, handle string) (string, error) {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		claudeSessionRecord(t, os.Getppid(), claudeSession)
		return captureStdout(t, func() error { return runCoopJoin([]string{"--me", handle}) })
	}
	// adopted runs the adoption every amq command starts with, and returns
	// the handle and root the command would use.
	adopted := func(claudeSession string) (string, string) {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		defer func() {
			for _, key := range identityEnvKeys {
				_ = os.Unsetenv(key)
			}
		}()
		if err := adoptClaudeSessionIdentity(); err != nil {
			t.Fatal(err)
		}
		return os.Getenv(envMe), os.Getenv(envRoot)
	}

	out, err := join(first, "reviewer")
	if err != nil || !strings.Contains(out, "Joined as reviewer (session "+defaultSessionName) {
		t.Fatalf("join = %q, %v; want the handle and the session", out, err)
	}
	// A handle that only join created is a registered agent: a strict send
	// to it is not refused.
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "reviewer", "--strict", "--body", "hi"})
	}); err != nil {
		t.Fatalf("strict send to the joined handle: %v", err)
	}
	if me, got := adopted(first); me != "reviewer" || !sameCleanPath(got, root) {
		t.Fatalf("adopted me=%q root=%q, want reviewer in %s", me, got, root)
	}
	if me, _ := adopted(second); me != "" {
		t.Fatalf("a session that did not join adopted handle %q", me)
	}
	// The first session still runs, in another process.
	claudeSessionRecord(t, os.Getpid(), first)
	if _, err := join(second, "reviewer"); GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), first) {
		t.Fatalf("second session took a held handle: %v, want action required naming session %s", err, first)
	}
	if out, err := join(second, "builder"); err != nil || !strings.Contains(out, "Joined as builder") {
		t.Fatalf("second session with its own handle = %q, %v", out, err)
	}

	t.Setenv(envMe, "claude")
	if _, err := join(first, "reviewer"); GetExitCode(err) != ExitActionRequired {
		t.Fatalf("join with AM_ME in the environment: %v, want action required", err)
	}
	setOptionalEnv(t, envMe, "", false)

	t.Setenv(envClaudeSession, first)
	if out, err := captureStdout(t, func() error { return runCoopLeave(nil) }); err != nil || !strings.Contains(out, "reviewer") {
		t.Fatalf("leave = %q, %v", out, err)
	}
	if me, _ := adopted(first); me != "" {
		t.Fatalf("a session that left still adopts handle %q", me)
	}
}

func TestCoopJoinTakesTheHandleOfASessionThatEnded(t *testing.T) {
	clearSendMailboxTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Chdir(t.TempDir())
	if _, err := captureStdout(t, func() error { return runCoopInit([]string{"--agents", "claude,codex", "--no-gitignore"}) }); err != nil {
		t.Fatal(err)
	}
	const ended, next = "7c29e148-77eb-47c0-8984-10c13b4700b1", "7c29e148-77eb-47c0-8984-10c13b4700b2"
	// join runs the command inside claudeSession: the one Claude Code
	// process of this test now runs that session, so a session that joined
	// before has no record any more, as after /clear.
	join := func(claudeSession string) error {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		claudeSessionRecord(t, os.Getppid(), claudeSession)
		_, err := captureStdout(t, func() error { return runCoopJoin([]string{"--me", "reviewer"}) })
		return err
	}
	if err := join(ended); err != nil {
		t.Fatal(err)
	}
	if err := join(next); err != nil {
		t.Fatalf("join with the handle of an ended session: %v", err)
	}
	t.Setenv(envClaudeSession, next)
	if err := adoptClaudeSessionIdentity(); err != nil || os.Getenv(envMe) != "reviewer" {
		t.Fatalf("the new session adopted %q, %v; want reviewer", os.Getenv(envMe), err)
	}
	for _, key := range identityEnvKeys {
		_ = os.Unsetenv(key)
	}
	// The ended session, if it comes back, must not act under the handle
	// and must not read as a session that never joined.
	t.Setenv(envClaudeSession, ended)
	if err := adoptClaudeSessionIdentity(); GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), next) {
		t.Fatalf("the ended session's adoption = %v, want a context mismatch naming session %s", err, next)
	}
	// Its leave must not free the handle the new session holds, nor say so.
	out, err := captureStdout(t, func() error { return runCoopLeave(nil) })
	if err != nil || strings.Contains(out, "is free") {
		t.Fatalf("the ended session's leave = %q, %v; want no claim that the handle is free", out, err)
	}
	t.Setenv(envClaudeSession, next)
	if err := adoptClaudeSessionIdentity(); err != nil || os.Getenv(envMe) != "reviewer" {
		t.Fatalf("after the ended session's leave the new session adopted %q, %v; want reviewer", os.Getenv(envMe), err)
	}
}
