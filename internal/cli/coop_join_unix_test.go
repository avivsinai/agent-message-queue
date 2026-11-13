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
// held it and nothing could run amq coop leave there. The third test is the
// regression test for two hints seen in a desktop-app session that named a
// fix which does not work there: a command with no handle said to export
// AM_ME, which does not last between the shell calls of a Claude Code
// session, and a join in a project that is not set up said to run amq
// session create, which refuses for the same reason. Pre-merge review added
// the states in which the new hints would again name a refused command: a
// session with an AM_* variable, where join refuses, and a project file
// whose queue does not exist yet, where coop init refuses and session
// create works. The fourth test breaks if a join with no --me stops taking,
// in this order, the handle the session holds, the name its user gave it
// when that is a handle, and claude; pre-merge review added that the name
// never gives the human's handle, that a binding which cannot be read is
// refused and not replaced, and that --me is used as normalized. The
// fifth test breaks if a chat of a host application loses its handle when
// /clear gives it a new Claude session, or if a Claude Code session started
// inside the chat, which inherits the chat's variables, gets the chat's
// handle. The sixth test breaks if a Codex thread cannot join and then act
// under the handle with no AM_* variable, if a thread whose writer lock is
// held loses its handle to another thread, if a thread whose lock is
// released keeps it, if a thread that joined again is refused for its own
// running wake, if a handle bound to a Claude session can be bound to a
// Codex thread too, or if a thread that left does not read as one that
// never joined.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// clearJoinTestEnv is clearSendMailboxTestEnv for a test that decides in
// which harness its commands run.
func clearJoinTestEnv(t *testing.T) {
	t.Helper()
	clearSendMailboxTestEnv(t)
	setOptionalEnv(t, envClaudeSession, "", false)
	setOptionalEnv(t, "CODEX_THREAD_ID", "", false)
}

// holdCodexThread takes the writer lock Codex holds while thread is loaded
// under codexHome, and returns the function that unloads it.
func holdCodexThread(t *testing.T, codexHome, thread string) func() {
	t.Helper()
	path := codexWriterLockPath(codexHome, thread)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return func() { _ = f.Close() }
}

// claudeSessionRecord writes the session record a Claude Code process pid
// keeps while it runs claudeSession, and returns its path. With the pid of
// this test's parent process, the test runs "inside" that session; with
// the pid of the test itself, the session runs somewhere else.
func claudeSessionRecord(t *testing.T, pid int, claudeSession string) string {
	t.Helper()
	return claudeSessionRecordNamed(t, pid, claudeSession, "", "")
}

// claudeChatRecord is claudeSessionRecord for a session that is the chat
// of a host application, as Claude Code records it under the desktop app. With chat "" the session is one started inside a chat.
func claudeChatRecord(t *testing.T, pid int, claudeSession, chat string) {
	t.Helper()
	path := claudeSessionRecord(t, pid, claudeSession)
	record := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"hostSessionId":%q,"pidDomain":%q}`, pid, claudeSession, chat, runtime.GOOS)
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeSessionRecordNamed is claudeSessionRecord for a session with a
// name; nameSource says who chose it ("user", or "derived" for a name
// Claude Code made from the directory).
func claudeSessionRecordNamed(t *testing.T, pid int, claudeSession, name, nameSource string) string {
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
	record := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":%q,"nameSource":%q,"pidDomain":%q}`, pid, claudeSession, name, nameSource, runtime.GOOS)
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoopJoinBindsTheSessionAndCommandsAdoptIt(t *testing.T) {
	clearJoinTestEnv(t)
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
			adoptedClaudeBinding = nil
		}()
		if err := adoptClaudeSessionIdentity(); err != nil {
			t.Fatal(err)
		}
		return os.Getenv(envMe), os.Getenv(envRoot)
	}

	out, err := join(first, "reviewer")
	if err != nil || !strings.Contains(out, "Joined as reviewer (session "+defaultSessionName) || !strings.Contains(out, "No wake: the wait hook is not installed") {
		t.Fatalf("join = %q, %v; want the handle, the session, and the missing wait hook", out, err)
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
	clearJoinTestEnv(t)
	// The adoptions below set the process's adopted binding; a later test
	// must not see a binding of this test's home.
	t.Cleanup(func() { adoptedClaudeBinding = nil })
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
	join := func(claudeSession string) (string, error) {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		claudeSessionRecord(t, os.Getppid(), claudeSession)
		return captureStdout(t, func() error { return runCoopJoin([]string{"--me", "reviewer"}) })
	}
	if out, err := join(ended); err != nil || strings.Contains(out, "was held by") {
		t.Fatalf("first join = %q, %v; want no handle taken from another session", out, err)
	}
	// The session that ended may be a chat that comes back, so the join
	// says whose handle it took.
	if out, err := join(next); err != nil || !strings.Contains(out, "was held by Claude session "+ended) {
		t.Fatalf("join with the handle of an ended session = %q, %v; want it to name session %s", out, err, ended)
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
	if err := adoptClaudeSessionIdentity(); GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), next) || !strings.Contains(err.Error(), "amq coop join --me") {
		t.Fatalf("the ended session's adoption = %v, want a context mismatch naming session %s and the join that works there", err, next)
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

func TestAClaudeSessionWithNoHandleIsToldTheFixThatWorksThere(t *testing.T) {
	clearJoinTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := requireMe(""); err == nil || !strings.Contains(err.Error(), "export AM_ME") {
		t.Fatalf("outside Claude Code the hint is the export, got: %v", err)
	}

	const claudeSession = "7c29e148-77eb-47c0-8984-10c13b4700b1"
	t.Setenv(envClaudeSession, claudeSession)
	claudeSessionRecord(t, os.Getppid(), claudeSession)
	if err := requireMe(""); err == nil || !strings.Contains(err.Error(), "amq coop join") || strings.Contains(err.Error(), "export") {
		t.Fatalf("in a Claude Code session the hint is the join, got: %v", err)
	}
	err := runCoopJoin([]string{"--me", "reviewer"})
	if GetExitCode(err) != ExitNotFound || !strings.Contains(err.Error(), "amq coop init") || strings.Contains(err.Error(), "session create") {
		t.Fatalf("a join in a project that is not set up names amq coop init, got: %v", err)
	}
	// A fresh clone: the project file is there, its queue is not.
	if err := os.WriteFile(".amqrc", []byte(`{"root": ".queue"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)
	err = runCoopJoin([]string{"--me", "reviewer"})
	if GetExitCode(err) != ExitNotFound || !strings.Contains(err.Error(), "amq session create collab") || strings.Contains(err.Error(), "coop init") {
		t.Fatalf("a join in a project whose queue does not exist yet names amq session create, got: %v", err)
	}
	if _, err := captureStdout(t, func() error { return runSessionCreate([]string{"collab"}) }); err != nil {
		t.Fatalf("the command the hint names: %v", err)
	}
	if _, err := captureStdout(t, func() error { return runCoopJoin([]string{"--me", "reviewer"}) }); err != nil {
		t.Fatalf("join after the hint's command: %v", err)
	}

	// Join refuses a session that has an AM_* variable, so the hint is the
	// plain one there.
	t.Setenv(envRoot, filepath.Join(t.TempDir(), "elsewhere"))
	if err := requireMe(""); err == nil || strings.Contains(err.Error(), "amq coop join") {
		t.Fatalf("a session with an AM_* variable is not sent to the join, got: %v", err)
	}
}

func TestCoopJoinWithoutAHandleTakesTheSessionNameOrClaude(t *testing.T) {
	clearJoinTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if _, err := captureStdout(t, func() error { return runCoopInit([]string{"--no-gitignore"}) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adoptedClaudeBinding = nil })
	join := func(claudeSession, name, nameSource string, args ...string) string {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		claudeSessionRecordNamed(t, os.Getppid(), claudeSession, name, nameSource)
		out, err := captureStdout(t, func() error { return runCoopJoin(args) })
		if err != nil {
			t.Fatalf("join in session named %q with %v: %v", name, args, err)
		}
		return out
	}
	const (
		titled  = "7c29e148-77eb-47c0-8984-10c13b4700b1"
		derived = "7c29e148-77eb-47c0-8984-10c13b4700b4"
		human   = "7c29e148-77eb-47c0-8984-10c13b4700b5"
		spaced  = "7c29e148-77eb-47c0-8984-10c13b4700b6"
		named   = "7c29e148-77eb-47c0-8984-10c13b4700b2"
		kept    = "7c29e148-77eb-47c0-8984-10c13b4700b3"
	)
	for _, tc := range []struct {
		session, name, nameSource string
		args                      []string
		want                      string
	}{
		// A title is a sentence; a derived name is a handle nobody chose.
		{titled, "Shell command execution", "user", nil, "Joined as claude (session collab, root "},
		{derived, "project-c2", "derived", nil, "Joined as claude (session collab, root "},
		// The human's handle is never a default.
		{human, "User", "user", nil, "Joined as claude (session collab, root "},
		{spaced, "", "", []string{"--me", " spacey "}, "Joined as spacey (session collab, root "},
		{named, "Reviewer", "user", nil, "Joined as reviewer (session collab, root "},
		{kept, "Reviewer", "user", []string{"--me", "builder"}, "Joined as builder (session collab, root "},
		{kept, "Reviewer", "user", nil, "Joined as builder (session collab, root "},
	} {
		if out := join(tc.session, tc.name, tc.nameSource, tc.args...); !strings.HasPrefix(out, tc.want) {
			t.Fatalf("join in session named %q with %v printed %q, want prefix %q", tc.name, tc.args, out, tc.want)
		}
	}

	// The session of the last case holds builder. With its holder marker
	// gone the binding cannot be read, and a bare join must not replace it
	// with a default.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	markers, err := filepath.Glob(filepath.Join(home, ".amq", "claude-sessions", "current", "*.json"))
	if err != nil || len(markers) == 0 {
		t.Fatalf("holder markers: %v %v", markers, err)
	}
	for _, marker := range markers {
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	err = runCoopJoin(nil)
	if GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "--me <handle>") {
		t.Fatalf("a bare join over a binding that cannot be read: %v, want a refusal that names --me", err)
	}
	if out, err := captureStdout(t, func() error { return runCoopJoin([]string{"--me", "builder"}) }); err != nil || !strings.HasPrefix(out, "Joined as builder ") {
		t.Fatalf("an explicit --me repairs the binding: %q %v", out, err)
	}
}

func TestAChatKeepsItsHandleWhenClearStartsANewClaudeSession(t *testing.T) {
	clearJoinTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if _, err := captureStdout(t, func() error { return runCoopInit([]string{"--no-gitignore"}) }); err != nil {
		t.Fatal(err)
	}
	const chat = "local_7c29e148-77eb-47c0-8984-10c13b4700c0"
	const before, after, inside = "7c29e148-77eb-47c0-8984-10c13b4700b1", "7c29e148-77eb-47c0-8984-10c13b4700b2", "7c29e148-77eb-47c0-8984-10c13b4700b3"
	// handle runs the adoption every amq command starts with, in a session
	// whose Claude Code process recorded chatInRecord.
	handle := func(claudeSession, chatInRecord string) string {
		t.Helper()
		t.Setenv(envClaudeSession, claudeSession)
		t.Setenv(envClaudeHostSession, chat)
		claudeChatRecord(t, os.Getppid(), claudeSession, chatInRecord)
		defer func() {
			for _, key := range identityEnvKeys {
				_ = os.Unsetenv(key)
			}
			adoptedClaudeBinding = nil
		}()
		if err := adoptClaudeSessionIdentity(); err != nil {
			t.Fatal(err)
		}
		return os.Getenv(envMe)
	}

	t.Setenv(envClaudeSession, before)
	claudeChatRecord(t, os.Getppid(), before, chat)
	if _, err := captureStdout(t, func() error { return runCoopJoin([]string{"--me", "reviewer"}) }); err != nil {
		t.Fatal(err)
	}
	if got := handle(after, chat); got != "reviewer" {
		t.Fatalf("the chat's next Claude session has handle %q, want reviewer with no join", got)
	}
	if got := handle(inside, ""); got != "" {
		t.Fatalf("a session started inside the chat has handle %q, want none", got)
	}
}

func TestCoopJoinBindsACodexThreadAndItsWriterLockDecidesTakeover(t *testing.T) {
	clearJoinTestEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	project := t.TempDir()
	t.Chdir(project)
	if _, err := captureStdout(t, func() error { return runCoopInit([]string{"--agents", "claude,codex", "--no-gitignore"}) }); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(project, defaultCoopRoot, defaultSessionName)
	const first, second = "019a1166-dd8e-77c0-bc3b-a4b7e24e91a1", "019a1166-dd8e-77c0-bc3b-a4b7e24e91a2"
	join := func(thread string, args ...string) (string, error) {
		t.Helper()
		t.Setenv("CODEX_THREAD_ID", thread)
		return captureStdout(t, func() error { return runCoopJoin(args) })
	}
	// adopted runs the adoption every amq command starts with in thread.
	adopted := func(thread string) (string, string, error) {
		t.Helper()
		t.Setenv("CODEX_THREAD_ID", thread)
		defer func() {
			for _, key := range identityEnvKeys {
				_ = os.Unsetenv(key)
			}
		}()
		err := adoptCodexThreadIdentity()
		return os.Getenv(envMe), os.Getenv(envRoot), err
	}

	unloadFirst := holdCodexThread(t, codexHome, first)
	out, err := join(first)
	if err != nil || !strings.HasPrefix(out, "Joined as codex (session "+defaultSessionName) || !strings.Contains(out, "No wake: run amq drain --include-body between tasks.") {
		t.Fatalf("join = %q, %v; want the default handle codex and no wake", out, err)
	}
	if me, got, err := adopted(first); err != nil || me != "codex" || !sameCleanPath(got, root) {
		t.Fatalf("adopted me=%q root=%q err=%v, want codex in %s", me, got, err, root)
	}
	// The thread's own wake runs; joining again changes nothing.
	wakePath := writeWakeLockExactForTest(t, root, "codex", wakeLock{PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "codex"})
	if out, err := join(first); err != nil || !strings.Contains(out, "The running wake of codex announces new mail.") {
		t.Fatalf("join again with the thread's own wake running = %q, %v; want no change and the wake named", out, err)
	}
	if err := os.Remove(wakePath); err != nil {
		t.Fatal(err)
	}

	holdCodexThread(t, codexHome, second)
	if _, err := join(second, "--me", "codex"); GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "running Codex thread "+first) {
		t.Fatalf("second thread took a handle held by a loaded thread: %v, want action required naming %s", err, first)
	}
	unloadFirst()
	if out, err := join(second, "--me", "codex"); err != nil || !strings.Contains(out, "was held by Codex thread "+first) {
		t.Fatalf("join with the handle of an unloaded thread = %q, %v; want it to name thread %s", out, err, first)
	}
	if _, _, err := adopted(first); GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), second) || !strings.Contains(err.Error(), "amq coop join --me") {
		t.Fatalf("the unloaded thread's adoption = %v, want a context mismatch naming %s and the join", err, second)
	}

	// A handle bound to a Claude session is not bound to a Codex thread too.
	const claudeSession = "7c29e148-77eb-47c0-8984-10c13b4700b1"
	setOptionalEnv(t, "CODEX_THREAD_ID", "", false)
	t.Setenv(envClaudeSession, claudeSession)
	claudeSessionRecord(t, os.Getppid(), claudeSession)
	if _, err := captureStdout(t, func() error { return runCoopJoin([]string{"--me", "reviewer"}) }); err != nil {
		t.Fatal(err)
	}
	setOptionalEnv(t, envClaudeSession, "", false)
	if _, err := join(second, "--me", "reviewer"); GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "bound to Claude session "+claudeSession) {
		t.Fatalf("a Codex thread took a handle bound to a Claude session: %v", err)
	}

	t.Setenv("CODEX_THREAD_ID", second)
	if out, err := captureStdout(t, func() error { return runCoopLeave(nil) }); err != nil || out != "Left: codex in "+root+" is free.\n" {
		t.Fatalf("leave = %q, %v", out, err)
	}
	if me, _, err := adopted(second); err != nil || me != "" {
		t.Fatalf("a thread that left adopted %q, %v; want the context of a thread that never joined", me, err)
	}
}
